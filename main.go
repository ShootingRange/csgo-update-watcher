package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const CSGO_CONTAINER_FILES = "./csgo-container"

type Config struct {
	ImageName string `json:"image_name"`
}

type UpdateWatcher struct {
	ctx           context.Context
	BaseImageName string
	// TODO CheckerImageName string // Image used for check latest version on steam
	dockerCli        *client.Client
	checkFrequency   time.Duration
	buildContextFile string
}

func main() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	updateWatcher := New("csgo-watched", cli, time.Minute*5)
	if err := updateWatcher.Start(false); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func New(baseImageName string, dockerCli *client.Client, checkFrequency time.Duration) *UpdateWatcher {
	return &UpdateWatcher{
		context.Background(),
		baseImageName,
		dockerCli,
		checkFrequency,
		"",
	}
}

func (this *UpdateWatcher) Start(stopOnError bool) error {
	err := this.createBuildContext()
	if err != nil {
		return fmt.Errorf("failed to create build context tar: %w", err)
	}

	err = this.ensureBaseImage()
	if err != nil {
		return fmt.Errorf("failed to ensure base image exists: %w", err)
	}

	// Enter main loop
	return this.watchAndBuild(stopOnError)
}

func (this *UpdateWatcher) createBuildContext() error {
	file, err := ioutil.TempFile(os.TempDir(), "csgo-update-watcher-")
	if err != nil {
		return fmt.Errorf("failed to create temp file for build context tar: %w", err)
	}
	log.Debug().Str("path", file.Name()).Msg("Created context.tar")

	tw := tar.NewWriter(file)
	defer func() { must(tw.Close()) }()

	walkRoot, err := filepath.Abs(CSGO_CONTAINER_FILES)
	if err != nil {
		return fmt.Errorf("failed to get absolute path of build context: %w", err)
	}

	err = filepath.Walk(walkRoot, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}

		if !info.Mode().IsRegular() || info.IsDir() {
			return nil
		}

		header := &tar.Header{
			Name: path[len(walkRoot)+1:],
			Mode: 0777,
			Size: info.Size(),
		}
		err := tw.WriteHeader(header)
		if err != nil {
			return fmt.Errorf("failed to write header in build context tar: %w", err)
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open file from build context: %w", err)
		}
		written, err := io.Copy(tw, file)
		if err != nil {
			return fmt.Errorf("failed to write file from build context into build context tar: %w", err)
		}
		if written < info.Size() {
			panic(fmt.Errorf("failed to write entire file from build context into build context tar: %w", err))
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("build context tar failed to build: %w", err)
	}

	this.buildContextFile = file.Name()

	return nil
}

func (this *UpdateWatcher) watchAndBuild(stopOnError bool) error {
	ticker := time.NewTicker(this.checkFrequency)
	for range ticker.C {
		latestVersion, err := this.latestVersion()
		if err != nil {
			log.Err(err).Msg("Failed to get latest version from Steam")
			if stopOnError {
				return err
			} else {
				continue
			}
		}
		log.Debug().Int("latest-version", latestVersion).Msg("Latest CS:GO buildid")
		newestBuildVersion, err := this.newestBuildVersion()
		if err != nil {
			log.Err(err).Msg("Failed to get buildid of newest build CS:GO container")
			if stopOnError {
				return err
			} else {
				continue
			}
		}
		log.Debug().Int("newest-build-version", newestBuildVersion).Msg("Newest CS:GO buildid with build container image")

		if newestBuildVersion < latestVersion {
			containerImage, buildid, err := this.buildContainerAndPublish()
			if err != nil {
				log.Err(err).Msg("Failed to build container image with latest CS:GO version")
				if stopOnError {
					return err
				} else {
					continue
				}
			}
			log.Info().
				Str("container-image", containerImage).
				Int("builid", buildid).
				Msg("Build new CS:GO container image")
		} else if newestBuildVersion > latestVersion {
			log.Warn().
				Int("steam-version", latestVersion).
				Int("local-version", newestBuildVersion).
				Msg("Docker host contains CS:GO container with newer version than Steam provides")
		}
	}

	return nil
}

// Retrieve the latest buildid/version from Steam
func (this *UpdateWatcher) latestVersion() (int, error) {
	// Start base image running the helper-latest-version.sh script
	panic("Not implemented")
}

// Get the buildid of newest version of CS:GO that the host have a container image of
func (this *UpdateWatcher) newestBuildVersion() (int, error) {
	// Check list of container images on Docker host and extract buildid from tag
	panic("Not implemented")
}

// Build a new container image with the latest version installed, and tag it with the buildid.
// Returns the container image name.
func (this *UpdateWatcher) buildContainerAndPublish() (string, int, error) {
	// TODO build CS:GO container image with game preinstalled

	// TODO run build container to determine buildid of installed version, use helper-installed-buildid.sh

	// TODO tag container with buildid

	panic("Not implemented")
}

// Build the base image if it is not present on the docker host
func (this *UpdateWatcher) ensureBaseImage() error {
	_, _, err := this.dockerCli.ImageInspectWithRaw(this.ctx, this.BaseImageName + ":base")
	if err != nil {
		// TODO determine if the base image is absent
		return fmt.Errorf("failed to check for base image: %w", err)
	}

	contextTar, err := os.Open(this.buildContextFile)
	if err != nil {
		return fmt.Errorf("failed to open build context tar: %w", err)
	}
	defer must(contextTar.Close())

	tag := this.BaseImageName + ":base"
	buildResp, err := this.dockerCli.ImageBuild(this.ctx, contextTar, types.ImageBuildOptions{
		Tags: []string{tag},
		NoCache: true,
		Dockerfile: "Dockerfile",
	})
	if err != nil {
		return fmt.Errorf("failed to build cs:go container: %w", err)
	}
	defer must(buildResp.Body.Close())

	return nil
}
