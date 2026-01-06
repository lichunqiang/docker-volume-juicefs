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
	logrus.Infof("mount command: %s", mount.String())

	// Use a channel to capture mount command errors
	mountErr := make(chan error, 1)
	go func() {
		output, err := mount.CombinedOutput()
		if err != nil {
			logrus.WithError(err).Errorf("mount command failed: %s", string(output))
			mountErr <- err
		} else {
			logrus.Infof("mount output: %s", string(output))
			mountErr <- nil
		}
	}()

	// Check for immediate startup errors (mount command failing to start)
	select {
	case err := <-mountErr:
		if err != nil {
			logrus.WithError(err).Errorf("failed to mount %s", v.Name)
			return err
		}
		// If mount command exits immediately without error, verify mountpoint below
	case <-time.After(100 * time.Millisecond):
		// Mount command is running (expected for daemon mode), continue to poll mountpoint
	}

	var fileinfo os.FileInfo
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		// Check if mount command has failed
		select {
		case err = <-mountErr:
			if err != nil {
				logrus.WithError(err).Errorf("failed to mount %s", v.Name)
				return err
			}
		default:
			// Mount command still running, continue
		}

		if fileinfo, err = os.Lstat(v.Mountpoint); err == nil {
			stat, ok := fileinfo.Sys().(*syscall.Stat_t)
			if !ok {
				logrus.Errorf("mountpoint %s state error", v.Mountpoint)
				return fmt.Errorf("mountpoint %s state error", v.Mountpoint)
			}
			if stat.Ino == 1 {
				// Verify write access by creating a marker file
				markerFile := v.Mountpoint + "/.juicefs"
				if fp, createErr := os.Create(markerFile); createErr == nil {
					_ = fp.Close()
					logrus.Infof("Successfully mounted and verified %s at %s", v.Name, v.Mountpoint)
					return nil
				} else {
					logrus.WithError(createErr).Warnf("Failed to create marker file on attempt %d", attempt+1)
				}
			} else {
				logrus.Warnf("Mountpoint inode is %d (expected 1) on attempt %d", stat.Ino, attempt+1)
			}
		} else {
			logrus.WithError(err).Warnf("Mountpoint not ready on attempt %d", attempt+1)
		}
		time.Sleep(time.Second)
	}

	// Final check for mount command error
	select {
	case err = <-mountErr:
		if err != nil {
			logrus.WithError(err).Error("mount failed")
			return err
		}
	default:
		// Mount command still running, but mountpoint check failed
	}
	logrus.Errorf("failed to mount %s: %v", v.Name, err)
	return fmt.Errorf("failed to mount %s after 10 attempts: %w", v.Name, err)
}

func mountVolume(v *jfsVolume) error {
	fi, err := os.Lstat(v.Mountpoint)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(v.Mountpoint, 0755); err != nil {
			logrus.WithError(err).Errorf("failed to create mountpoint %s", v.Mountpoint)
			return err
		}
	} else if err != nil {
		logrus.WithError(err).Errorf("failed to stat mountpoint %s", v.Mountpoint)
		return err
	}

	if fi != nil && !fi.IsDir() {
		logrus.WithField("mountPoint", v.Mountpoint).Errorf("%s already exist and it's not a directory", v.Name)
		return fmt.Errorf("%v already exist and it's not a directory", v.Mountpoint)
	}

	return ceMount(v)
}

func umountVolume(v *jfsVolume) error {
	// remove .juicefs file
	_ = os.Remove(v.Mountpoint + "/.juicefs")
	cmd := exec.Command("umount", v.Mountpoint)
	logrus.Debug(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		logrus.WithError(err).Errorf("juicefs umount error: %s", out)
		return err
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
		return errors.New("'name' option required")
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
		logrus.Errorf("volume %s not found", r.Name)
		return fmt.Errorf("volume %s not found", r.Name)
	}

	if v.connections != 0 {
		logrus.Errorf("volume %s is in use, connections: %d", r.Name, v.connections)
		return fmt.Errorf("volume %s is in use, connections: %d", r.Name, v.connections)
	}

	if err := os.Remove(v.Mountpoint); err != nil {
		if !errors.Is(err, os.ErrNotExist) { // mountpoint not exist, it's ok
			logrus.WithError(err).Errorf("failed to remove mountpoint %s", v.Mountpoint)
			return err
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
		logrus.Infof("volume %s not exists", r.Name)
		return &volume.PathResponse{}, fmt.Errorf("volume %s not exists", r.Name)
	}

	return &volume.PathResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *jfsDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	logrus.WithField("method", "mount").Debugf("%#v", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		logrus.Infof("volume %s not exists", r.Name)
		return &volume.MountResponse{}, fmt.Errorf("volume %s not found", r.Name)
	}
	if v.connections == 0 {
		if err := mountVolume(v); err != nil {
			logrus.WithError(err).Errorf("failed to mount %s", r.Name)
			return &volume.MountResponse{}, err
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
		logrus.Errorf("volume %s not found", r.Name)
		return fmt.Errorf("volume %s not found", r.Name)
	}

	if v.connections <= 1 {
		if err := umountVolume(v); err != nil {
			logrus.WithError(err).Errorf("failed to umount %s", r.Name)
			return err
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
		logrus.Infof("volume %s not exists", r.Name)
		return &volume.GetResponse{}, fmt.Errorf("volume %s not exists", r.Name)
	}

	return &volume.GetResponse{Volume: &volume.Volume{
		Name:       r.Name,
		Mountpoint: v.Mountpoint,
		Status: map[string]interface{}{
			"connections": v.connections,
		},
	}}, nil
}

func (d *jfsDriver) List() (*volume.ListResponse, error) {
	logrus.WithField("method", "list").Debugf("")

	d.Lock()
	defer d.Unlock()

	var vols []*volume.Volume
	for name, v := range d.volumes {
		vols = append(vols, &volume.Volume{
			Name:       name,
			Mountpoint: v.Mountpoint,
			Status: map[string]interface{}{
				"connections": v.connections,
			}})
	}
	return &volume.ListResponse{Volumes: vols}, nil
}

func (d *jfsDriver) Capabilities() *volume.CapabilitiesResponse {
	logrus.WithField("method", "capabilities").Debugf("")

	return &volume.CapabilitiesResponse{Capabilities: volume.Capability{Scope: "local"}}
}

func main() {
	rotator := &lumberjack.Logger{
		Filename:   "/var/log/juicefs.log", // 日志文件路径
		MaxSize:    2,                      // 每个日志文件最大2MB
		MaxBackups: 5,                      // 最多保留5个旧日志文件
		MaxAge:     7,                      // 最多保留7天
		Compress:   true,                   // 是否压缩旧日志
	}

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

	// handle graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-c
		logrus.Infof("Received signal %v, initiating graceful shutdown", sig)

		// Save driver state
		logrus.Info("Saving driver state...")
		d.saveState()

		// Log active volumes
		d.RLock()
		if len(d.volumes) > 0 {
			logrus.Warnf("Shutting down with %d active volumes still mounted", len(d.volumes))
			for name, vol := range d.volumes {
				logrus.Warnf("  Volume %s: mountpoint=%s, connections=%d", name, vol.Mountpoint, vol.connections)
			}
		}
		d.RUnlock()

		// Rotate logs
		logrus.Info("Rotating logs...")
		_ = rotator.Rotate()

		logrus.Info("Shutdown complete")
		os.Exit(0)
	}()

	h := volume.NewHandler(d)
	logrus.Infof("listening on %s", socketAddress)
	logrus.Error(h.ServeUnix(socketAddress, 0))
}
