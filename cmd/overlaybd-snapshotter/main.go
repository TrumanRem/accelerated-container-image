package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	overlaybd "github.com/alibaba/accelerated-container-image/pkg/snapshot"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/contrib/snapshotservice"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

const defaultConfigPath = "/etc/overlaybd-snapshotter/config.json"

type pluginConfig struct {
	Address string `json:"address"`
	Root    string `json:"root"`
}

var pconfig pluginConfig

func parseConfig(fpath string) error {
	data, err := ioutil.ReadFile(fpath)
	if err != nil {
		return errors.Wrapf(err, "failed to read plugin config from %s", fpath)
	}

	if err := json.Unmarshal(data, &pconfig); err != nil {
		return errors.Wrapf(err, "failed to parse plugin config from %s", string(data))
	}
	return nil
}

// TODO: use github.com/urfave/cli
func main() {

	if err := parseConfig(defaultConfigPath); err != nil {
		logrus.Error(err)
		os.Exit(1)
	}

	sn, err := overlaybd.NewSnapshotter(pconfig.Root)
	if err != nil {
		logrus.Errorf("failed to init overlaybd snapshotter: %v", err)
		os.Exit(1)
	}
	defer sn.Close()

	srv := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(srv, snapshotservice.FromSnapshotter(sn))

	address := strings.TrimSpace(pconfig.Address)

	if address == "" {
		logrus.Errorf("invalid address path(%s)", address)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		logrus.Errorf("failed to create directory %v", filepath.Dir(address))
		os.Exit(1)
	}

	// try to remove the socket file to avoid EADDRINUSE
	if err := os.RemoveAll(address); err != nil {
		logrus.Errorf("failed to remove %v", address)
		os.Exit(1)
	}

	l, err := net.Listen("unix", address)
	if err != nil {
		logrus.Errorf("failed to listen on %s: %v", address, err)
		os.Exit(1)
	}

	go func() {
		if err := srv.Serve(l); err != nil {
			logrus.Errorf("failed to server: %v", err)
			os.Exit(1)
		}
	}()

	logrus.Infof("start to serve overlaybd snapshotter on %s", address)

	signals := make(chan os.Signal, 32)
	signal.Notify(signals, unix.SIGTERM, unix.SIGINT, unix.SIGPIPE)

	<-handleSignals(context.TODO(), signals, srv)
}

func handleSignals(ctx context.Context, signals chan os.Signal, server *grpc.Server) chan struct{} {
	doneCh := make(chan struct{}, 1)

	go func() {
		for {
			s := <-signals
			switch s {
			case unix.SIGUSR1:
				dumpStacks()
			case unix.SIGPIPE:
				continue
			default:
				if server == nil {
					close(doneCh)
					return
				}

				server.Stop()
				close(doneCh)
				return
			}
		}
	}()

	return doneCh
}

func dumpStacks() {
	var (
		buf       []byte
		stackSize int
	)

	bufferLen := 16384
	for stackSize == len(buf) {
		buf = make([]byte, bufferLen)
		stackSize = runtime.Stack(buf, true)
		bufferLen *= 2
	}

	buf = buf[:stackSize]
	logrus.Infof("=== BEGIN goroutine stack dump ===\n%s\n=== END goroutine stack dump ===", buf)
}