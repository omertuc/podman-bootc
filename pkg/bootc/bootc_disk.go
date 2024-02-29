package bootc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"podman-bootc/pkg/config"
	"syscall"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/domain/entities/types"
	"github.com/containers/podman/v5/pkg/specgen"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const diskSize = 10 * 1024 * 1024 * 1024
const imageMetaXattr = "user.bootc.meta"

var ctx context.Context = func() (ctx context.Context) {
	if _, err := os.Stat(config.MachineSocket); err != nil {
		logrus.Errorf("podman machine socket is missing. Is podman machine running?\n%s", err)
		os.Exit(1)
		return
	}

	ctx, err := bindings.NewConnectionWithIdentity(
		context.Background(),
		fmt.Sprintf("unix://%s", config.MachineSocket),
		config.MachineSshKeyPriv,
		true)
	if err != nil {
		logrus.Errorf("failed to connect to the podman socket. Is podman machine running?\n%s", err)
		os.Exit(1)
		return
	}

	return ctx
}()

// diskFromContainerMeta is serialized to JSON in a user xattr on a disk image
type diskFromContainerMeta struct {
	// imageDigest is the digested sha256 of the container that was used to build this disk
	ImageDigest string `json:"imageDigest"`
}

type BootcDisk struct {
	Image     string
	file      *os.File
	directory string
	digest    string
}

func (p *BootcDisk) GetDirectory() string {
	return p.directory
}

func (p *BootcDisk) GetDigest() string {
	return p.digest
}

func (p *BootcDisk) Install() (err error) {
	start := time.Now()

	err = p.pullImage()
	if err != nil {
		return
	}

	err = p.getOrInstallImageToDisk()
	if err != nil {
		return
	}

	elapsed := time.Since(start)
	logrus.Debugf("installImage elapsed: %v", elapsed)

	return
}

// getOrInstallImageToDisk checks if the disk is present and if not, installs the image to a new disk
func (p *BootcDisk) getOrInstallImageToDisk() error {
	diskPath := filepath.Join(p.directory, config.DiskImage)
	f, err := os.Open(diskPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return p.bootcInstallImageToDisk()
	}
	defer f.Close()
	buf := make([]byte, 4096)
	len, err := unix.Fgetxattr(int(f.Fd()), imageMetaXattr, buf)
	if err != nil {
		// If there's no xattr, just remove it
		os.Remove(diskPath)
		return p.bootcInstallImageToDisk()
	}
	bufTrimmed := buf[:len]
	var serializedMeta diskFromContainerMeta
	if err := json.Unmarshal(bufTrimmed, &serializedMeta); err != nil {
		logrus.Warnf("failed to parse serialized meta from %s (%v) %v", diskPath, buf, err)
		return p.bootcInstallImageToDisk()
	}

	logrus.Debugf("previous disk digest: %s current digest: %s", serializedMeta.ImageDigest, p.digest)
	if serializedMeta.ImageDigest == p.digest {
		return nil
	}

	return p.bootcInstallImageToDisk()
}

// bootcInstallImageToDisk creates a disk image from a bootc container
func (p *BootcDisk) bootcInstallImageToDisk() (err error) {
	p.file, err = os.CreateTemp(p.directory, "podman-bootc-tempdisk")
	if err != nil {
		return err
	}
	if err := syscall.Ftruncate(int(p.file.Fd()), diskSize); err != nil {
		return err
	}
	doCleanupDisk := true
	defer func() {
		if doCleanupDisk {
			os.Remove(p.file.Name())
		}
	}()

	err = p.runInstallContainer()
	if err != nil {
		return fmt.Errorf("failed to create disk image: %w", err)
	}
	serializedMeta := diskFromContainerMeta{
		ImageDigest: p.digest,
	}
	buf, err := json.Marshal(serializedMeta)
	if err != nil {
		return err
	}
	if err := unix.Fsetxattr(int(p.file.Fd()), imageMetaXattr, buf, 0); err != nil {
		return fmt.Errorf("failed to set xattr: %w", err)
	}
	diskPath := filepath.Join(p.directory, config.DiskImage)

	if err := os.Rename(p.file.Name(), diskPath); err != nil {
		return fmt.Errorf("failed to rename to %s: %w", diskPath, err)
	}
	doCleanupDisk = false

	return nil
}

// pullImage fetches the container image if not present
func (p *BootcDisk) pullImage() (err error) {
	pullPolicy := "missing"
	ids, err := images.Pull(ctx, p.Image, &images.PullOptions{Policy: &pullPolicy})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	if len(ids) == 0 {
		return fmt.Errorf("no ids returned from image pull")
	}

	if len(ids) > 1 {
		return fmt.Errorf("multiple ids returned from image pull")
	}

	_, err = images.GetImage(ctx, p.Image, &images.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get image: %w", err)
	}

	imageId := ids[0]

	// Create VM cache dir; one per oci bootc image
	p.directory = filepath.Join(config.CacheDir, imageId)
	if err := os.MkdirAll(p.directory, os.ModePerm); err != nil {
		return fmt.Errorf("error while making bootc disk directory: %w", err)
	}

	return
}

// runInstallContainer runs the bootc installer in a container to create a disk image
func (p *BootcDisk) runInstallContainer() (err error) {
	createResponse, err := p.createInstallContainer()
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	// run the container to create the disk
	err = containers.Start(ctx, createResponse.ID, &containers.StartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// stream logs to stdout and stderr
	stdOut := make(chan string)
	stdErr := make(chan string)
	logErrors := make(chan error)
	defer close(stdOut)
	defer close(stdErr)

	go func() {
		follow := true
		err = containers.Logs(ctx, createResponse.ID, &containers.LogOptions{Follow: &follow}, stdOut, stdErr)
		if err != nil {
			logErrors <- err
		}

		close(logErrors)
	}()

	streamToStdout(stdOut)
	streamToStdout(stdErr)

	//wait for the container to finish
	exitCode, err := containers.Wait(ctx, createResponse.ID, nil)
	if err != nil {
		return fmt.Errorf("failed to wait for container: %w", err)
	}

	if err := <-logErrors; err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	if exitCode != 0 {
		return fmt.Errorf("failed to run bootc install")
	}

	return
}

func streamToStdout(stream chan string) {
	go func () {
		for str := range stream {
			fmt.Print(str)
		}
	}()
}

// streamLogs streams the logs from the container to stdout and stderr
func (p *BootcDisk) streamLogs(containerId string) (err error) {
	return
}

// createInstallContainer creates a container to run the bootc installer
func (p *BootcDisk) createInstallContainer() (createResponse types.ContainerCreateResponse, err error) {
	privileged := true
	autoRemove := true
	labelNested := true

	s := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Command: []string{
				"bootc", "install", "to-disk", "--via-loopback", "--generic-image",
				"--skip-fetch-check", "/output/" + filepath.Base(p.file.Name()),
			},
			PidNS:       specgen.Namespace{NSMode: specgen.Host},
			Remove:      &autoRemove,
			Annotations: map[string]string{"io.podman.annotations.label": "type:unconfined_t"},
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: p.Image,
			Mounts: []specs.Mount{
				{
					Source:      "/var/lib/containers",
					Destination: "/var/lib/containers",
					Type:        "bind",
				},
				{
					Source:      "/dev",
					Destination: "/dev",
					Type:        "bind",
				},
				{
					Source:      p.directory,
					Destination: "/output",
					Type:        "bind",
				},
			},
		},
		ContainerSecurityConfig: specgen.ContainerSecurityConfig{
			Privileged:  &privileged,
			LabelNested: &labelNested,
			SelinuxOpts: []string{"type:unconfined_t"},
		},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			NetNS: specgen.Namespace{
				NSMode: specgen.Bridge,
			},
		},
	}

	createResponse, err = containers.CreateWithSpec(ctx, s, &containers.CreateOptions{})
	if err != nil {
		return createResponse, fmt.Errorf("failed to create container: %w", err)
	}

	return
}