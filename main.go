package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/go-plugins-helpers/volume"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	socketAddress = "/run/docker/plugins/jfs.sock"
	ceCliPath     = "/usr/bin/juicefs"
)

type jfsVolume struct {
	Name        string
	Options     map[string]string
	Source      string
	Mountpoint  string
	connections int
}

type jfsDriver struct {
	sync.RWMutex

	root      string
	statePath string
	volumes   map[string]*jfsVolume
}

func newJfsDriver(root string) (*jfsDriver, error) {
	logrus.WithField("method", "newJfsDriver").Debug(root)

	d := &jfsDriver{
		root:      filepath.Join(root, "volumes"),
		statePath: filepath.Join(root, "state", "jfs-state.json"),
		volumes:   map[string]*jfsVolume{},
	}

	if data, err := os.ReadFile(d.statePath); err != nil {
		if os.IsNotExist(err) {
			logrus.WithField("statePath", d.statePath).Debug("no state found")
		} else {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, &d.volumes); err != nil {
			return nil, err
		}
	}

	return d, nil
}

func (d *jfsDriver) saveState() {
	data, err := json.Marshal(d.volumes)
	if err != nil {
		logrus.WithField("statePath", d.statePath).Error(err)
	}

	if err := os.WriteFile(d.statePath, data, 0600); err != nil {
		logrus.WithField("saveState", d.statePath).Error(err)
	}
}

func ceMount(v *jfsVolume) error {
	options := map[string]string{}
	mount := exec.Command(ceCliPath, "mount")
	for k, v := range v.Options {
		if k == "env" {
			mount.Env = append(os.Environ(), strings.Split(v, ",")...)
			logrus.Debugf("modified env: %v", mount.Env)
			continue
		}
		options[k] = v
	}

	mountFlags := []string{
		"cache-partial-only",
		"enable-xattr",
		"no-syslog",
		"no-usage-report",
		"writeback",
	}
	for _, mountFlag := range mountFlags {
		_, ok := options[mountFlag]
		if !ok {
			continue
		}
		mount.Args = append(mount.Args, fmt.Sprintf("--%s", mountFlag))
		delete(options, mountFlag)
	}
	for mountOption, val := range options {
		mount.Args = append(mount.Args, fmt.Sprintf("--%s=%s", mountOption, val))
	}
	mount.Args = append(mount.Args, v.Source, v.Mountpoint)
	logrus.Debugf("mount command: %s", mount.String())

	// Use a channel to capture mount command errors
	mountErr := make(chan error, 1)
	go func() {
		output, err := mount.CombinedOutput()
		if err != nil {
			logrus.Errorf("mount command failed: %s, error: %v", string(output), err)
			mountErr <- fmt.Errorf("mount command failed: %s: %v", string(output), err)
		} else {
			logrus.Infof("mount output: %s", string(output))
			mountErr <- nil
		}
	}()

	// Check for immediate startup errors (mount command failing to start)
	select {
	case err := <-mountErr:
		if err != nil {
			return logError("failed to mount %s: %v", v.Name, err)
		}
		// If mount command exits immediately without error, verify mountpoint below
	case <-time.After(100 * time.Millisecond):
		// Mount command is running (expected for daemon mode), continue to poll mountpoint
	}

	touch := exec.Command("touch", v.Mountpoint+"/.juicefs")
	var fileinfo os.FileInfo
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		// Check if mount command has failed
		select {
		case err := <-mountErr:
			if err != nil {
				return logError("failed to mount %s: %v", v.Name, err)
			}
		default:
			// Mount command still running, continue
		}

		if fileinfo, err = os.Lstat(v.Mountpoint); err == nil {
			stat, ok := fileinfo.Sys().(*syscall.Stat_t)
			if !ok {
				return logError("Not a syscall.Stat_t")
			}
			if stat.Ino == 1 {
				if err = touch.Run(); err == nil {
					return nil
				}
			}
		}
		logrus.Debugf("Error in attempt %d: %#v", attempt+1, err)
		time.Sleep(time.Second)
	}

	// Final check for mount command error
	select {
	case err := <-mountErr:
		if err != nil {
			return logError("failed to mount %s: %v", v.Name, err)
		}
	default:
		// Mount command still running, but mountpoint check failed
	}
	return logError("failed to mount %s: %v", v.Name, err)
}

func mountVolume(v *jfsVolume) error {
	fi, err := os.Lstat(v.Mountpoint)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(v.Mountpoint, 0755); err != nil {
			return logError(err.Error())
		}
	} else if err != nil {
		return logError(err.Error())
	}

	if fi != nil && !fi.IsDir() {
		return logError("%v already exist and it's not a directory", v.Mountpoint)
	}

	return ceMount(v)
}

func umountVolume(v *jfsVolume) error {
	// remove .juicefs file
	_ = os.Remove(v.Mountpoint + "/.juicefs")
	cmd := exec.Command("umount", v.Mountpoint)
	logrus.Debug(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		logrus.Errorf("juicefs umount error: %s", out)
		return logError(err.Error())
	} else {
		logrus.Infof("juicefs umount output: %s", string(out))
	}
	return nil
}

func (d *jfsDriver) Create(r *volume.CreateRequest) error {
	logrus.WithField("method", "create").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v := &jfsVolume{
		Options: map[string]string{},
	}

	for key, val := range r.Options {
		switch key {
		case "name":
			v.Name = val
		case "metaurl":
			v.Source = val
			if !strings.Contains(v.Source, "://") {
				// Default scheme of meta URL is redis://
				v.Source = "redis://" + v.Source
			}
		default:
			v.Options[key] = val
		}
	}

	if v.Name == "" {
		return logError("'name' option required")
	}
	if v.Source == "" {
		v.Source = v.Name
	}

	v.Mountpoint = filepath.Join(d.root, r.Name)
	d.volumes[r.Name] = v

	d.saveState()
	return nil
}

func (d *jfsDriver) Remove(r *volume.RemoveRequest) error {
	logrus.WithField("method", "remove").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]

	if !ok {
		return logError("volume %s not found", r.Name)
	}

	if v.connections != 0 {
		return logError("volume %s is in use, connections: %d", r.Name, v.connections)
	}

	if err := os.Remove(v.Mountpoint); err != nil {
		if !errors.Is(err, os.ErrNotExist) { // mountpoint not exist, it's ok
			return logError(err.Error())
		}
		logrus.Infof("mountpoint %s not exist, it's ok", v.Mountpoint)
	}

	delete(d.volumes, r.Name)
	d.saveState()
	return nil
}

func (d *jfsDriver) Path(r *volume.PathRequest) (*volume.PathResponse, error) {
	logrus.WithField("method", "path").Debugf("%#v", r)

	d.RLock()
	defer d.RUnlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.PathResponse{}, logError("volume %s not found", r.Name)
	}

	return &volume.PathResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *jfsDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	logrus.WithField("method", "mount").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.MountResponse{}, logError("volume %s not found", r.Name)
	}
	if v.connections == 0 {
		if err := mountVolume(v); err != nil {
			return &volume.MountResponse{}, logError("failed to mount %s: %s", r.Name, err)
		}
	}

	v.connections++
	return &volume.MountResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *jfsDriver) Unmount(r *volume.UnmountRequest) error {
	logrus.WithField("method", "umount").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()
	v, ok := d.volumes[r.Name]
	if !ok {
		return logError("volume %s not found", r.Name)
	}

	if v.connections <= 1 {
		if err := umountVolume(v); err != nil {
			return logError("failed to umount %s: %s", r.Name, err)
		}
		v.connections = 0
	} else {
		v.connections--
	}

	return nil
}

func (d *jfsDriver) Get(r *volume.GetRequest) (*volume.GetResponse, error) {
	logrus.WithField("method", "get").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.GetResponse{}, logError("volume %s not found", r.Name)
	}

	return &volume.GetResponse{Volume: &volume.Volume{Name: r.Name, Mountpoint: v.Mountpoint}}, nil
}

func (d *jfsDriver) List() (*volume.ListResponse, error) {
	logrus.WithField("method", "list").Debugf("")

	d.Lock()
	defer d.Unlock()

	var vols []*volume.Volume
	for name, v := range d.volumes {
		vols = append(vols, &volume.Volume{Name: name, Mountpoint: v.Mountpoint})
	}
	return &volume.ListResponse{Volumes: vols}, nil
}

func (d *jfsDriver) Capabilities() *volume.CapabilitiesResponse {
	logrus.WithField("method", "capabilities").Debugf("")

	return &volume.CapabilitiesResponse{Capabilities: volume.Capability{Scope: "local"}}
}

func logError(format string, args ...interface{}) error {
	logrus.Errorf(format, args...)
	return fmt.Errorf(format, args...)
}

func main() {
	rotator := &lumberjack.Logger{
		Filename:   "/var/log/juicefs.log", // 日志文件路径
		MaxSize:    2,                      // 每个日志文件最大2MB
		MaxBackups: 5,                      // 最多保留5个旧日志文件
		MaxAge:     7,                      // 最多保留7天
		Compress:   true,                   // 是否压缩旧日志
	}

	// handle graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-c
		logrus.Infof("Received signal %v, shutting down gracefully", sig)
		_ = rotator.Rotate()
		os.Exit(0)
	}()
	logrus.SetReportCaller(true)
	logrus.SetOutput(rotator)
	logrus.SetFormatter(&logrus.JSONFormatter{DisableHTMLEscape: true})

	debug := os.Getenv("DEBUG")
	if ok, _ := strconv.ParseBool(debug); ok {
		logrus.SetLevel(logrus.DebugLevel)
	}

	d, err := newJfsDriver("/jfs")
	if err != nil {
		logrus.Fatal(err)
	}
	h := volume.NewHandler(d)
	logrus.Infof("listening on %s", socketAddress)
	logrus.Error(h.ServeUnix(socketAddress, 0))
}
