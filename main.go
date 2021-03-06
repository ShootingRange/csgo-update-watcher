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
	"github.com/google/uuid"
	"github.com/gtuk/discordwebhook"
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

type UpdateWatcher struct {
	ctx              context.Context
	BaseImageName    string
	dockerCli        *client.Client
	checkFrequency   time.Duration
	buildContextFile string
	discordHook      string
}

func main() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	updateWatcher := New("csgo-watched", cli, time.Second*5, os.Getenv("DISCORD_HOOK"))
	if err := updateWatcher.Start(false); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func New(baseImageName string, dockerCli *client.Client, checkFrequency time.Duration, discordHook string) *UpdateWatcher {
	return &UpdateWatcher{
		context.Background(),
		baseImageName,
		dockerCli,
		checkFrequency,
		"",
		discordHook,
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

func (this *UpdateWatcher) announceNewVersion(buildid int) {
	if this.discordHook == "" {
		return
	}

	username := "CS:GO update watcher"
	content := "New CS:GO version released, buildid " + strconv.Itoa(buildid)
	err := discordwebhook.SendMessage(this.discordHook, discordwebhook.Message{
		Username: &username,
		Content:  &content,
	})
	if err != nil {
		log.Err(err).Msg("Failed to send new version announcement")
	}
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
			go this.announceNewVersion(latestVersion)

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

func (this *UpdateWatcher) runScript(script string, image string) (string, error) {
	// Start base image running the helper-latest-version.sh script

	containerConfig := &container.Config{
		Image:      image,
		Shell:      []string{"/bin/sh"},
		Cmd:        []string{script},
		Entrypoint: []string{"/bin/sh"},
	}
	hostConfig := &container.HostConfig{}
	result, err := this.dockerCli.ContainerCreate(this.ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("failed to create container for getting latest version on Steam: %w", err)
	}
	containerID := result.ID
	log.Trace().Msg("Created container")

	if err := this.dockerCli.ContainerStart(this.ctx, containerID, types.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start container that gets latest version from Steam: %w", err)
	}
	log.Trace().Msg("Started container")

	wait, errChan := this.dockerCli.ContainerWait(this.ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errChan:
		return "", fmt.Errorf("error while waiting for Steam version retriever container to stop: %w", err)
	case <-wait:
	}
	log.Trace().Msg("Container stopped container")

	// Read container logs
	logOptions := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: false,
	}
	logReader, err := this.dockerCli.ContainerLogs(this.ctx, containerID, logOptions)
	if err != nil {
		return "", fmt.Errorf("could not request logs from container: %w", err)
	}
	logBuffer := bytes.NewBuffer([]byte{})
	_, err = stdcopy.StdCopy(logBuffer, ioutil.Discard, logReader)
	if err != nil {
		return "", fmt.Errorf("error while demultiplexing container logs: %w", err)
	}
	logBytes, err := ioutil.ReadAll(logBuffer)
	if err != nil {
		return "", fmt.Errorf("could not read logs from container: %w", err)
	}
	logs := string(logBytes)

	// Remove container
	if err := this.dockerCli.ContainerRemove(this.ctx, containerID, types.ContainerRemoveOptions{}); err != nil {
		return "", fmt.Errorf("failed to remove container: %w", err)
	}

	return logs, nil
}

// Retrieve the latest buildid/version from Steam
func (this *UpdateWatcher) latestVersion() (int, error) {
	// Start base image running the helper-latest-version.sh script
	logs, err := this.runScript("/usr/src/helper-latest-buildid.sh", this.BaseImageName+":base")
	if err != nil {
		return 0, fmt.Errorf("failed to run script for checking latest CS:GO version on Steam: %w", err)
	}
	// NOTE strip trailing newline
	buildid, err := strconv.Atoi(logs[:len(logs)-1])
	if err != nil {
		log.Err(err).Str("logs", logs).Msg("Failed to parse buildid")
		return 0, fmt.Errorf("failed to parse buildid: %w", err)
	}

	return buildid, nil
}

// Get the buildid of newest version of CS:GO that the host have a container image of
func (this *UpdateWatcher) newestBuildVersion() (int, error) {
	// Check list of container images on Docker host and extract buildid from tag

	images, err := this.dockerCli.ImageList(this.ctx, types.ImageListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list images on docker host: %w", err)
	}

	expectedPrefix := this.BaseImageName + ":buildid-"
	largestBuildid := -1
	for _, image := range images {
		for _, tag := range image.RepoTags {
			// Check if prefix matches

			if len(tag) <= len(expectedPrefix) {
				continue
			}

			if tag[:len(expectedPrefix)] != expectedPrefix {
				continue
			}

			buildidStr := tag[len(expectedPrefix):]
			buildid, err := strconv.Atoi(buildidStr)
			if err != nil {
				log.Warn().Str("tag", tag).Msg("Failed to extract buildid from container tag")
			}

			if largestBuildid < buildid {
				largestBuildid = buildid
			}
		}
	}

	return largestBuildid, nil
}

// Build a new container image with the latest version installed, and tag it with the buildid.
// Returns the container image name.
func (this *UpdateWatcher) buildContainerAndPublish() (string, int, error) {
	log.Info().Msg("Building new CS:GO container")

	// build CS:GO container image with game preinstalled
	tempTag := this.BaseImageName + ":temp-" + uuid.NewString()
	err := this.buildContainer(
		this.BaseImageName+":base",
		tempTag,
		"Dockerfile-preinstall",
	)
	if err != nil {
		return "", 0, err
	}

	// run build container to determine buildid of installed version, use helper-installed-buildid.sh
	buildid, err := this.getImageBuildid(tempTag)
	if err != nil {
		return "", 0, err
	}

	// tag container with buildid
	taggedImage := this.BaseImageName + ":preinstall-buildid-" + strconv.Itoa(buildid)
	if err := this.dockerCli.ImageTag(this.ctx, tempTag, taggedImage); err != nil {
		return "", 0, fmt.Errorf("failed to tag newly build cs:go container with buildid: %w", err)
	}

	// build get5 container
	get5TaggedImage := this.BaseImageName + ":get5-buildid-" + strconv.Itoa(buildid)
	err = this.buildContainer(
		taggedImage,
		get5TaggedImage,
		"Dockerfile-get5",
	)
	if err != nil {
		return "", 0, err
	}

	/*
		if pushReader, err := this.dockerCli.ImagePush(this.ctx, taggedImage, types.ImagePushOptions{}); err != nil {
			return "", 0, fmt.Errorf("failed to push newly build cs:go container to registry: %w", err)
		} else {
			if _, err := io.Copy(ioutil.Discard, pushReader); err != nil {
				return "", 0, fmt.Errorf("failed to read reply from image push: %w", err)
			}
		}
	*/

	return taggedImage, buildid, nil
}

// Build the base image if it is not present on the docker host
func (this *UpdateWatcher) ensureBaseImage() error {
	tag := this.BaseImageName + ":base"

	_, _, err := this.dockerCli.ImageInspectWithRaw(this.ctx, tag)
	if err != nil {
		if client.IsErrNotFound(err) {
			// Build image
		} else {
			return fmt.Errorf("could not inspect base image: %w", err)
		}
	} else {
		// Base image already exists
		log.Trace().Msg("Base image already exists, not rebuilding")
		return nil
	}

	log.Info().Msg("Building base image")

	contextTar, err := os.Open(this.buildContextFile)
	if err != nil {
		return fmt.Errorf("failed to open build context tar: %w", err)
	}

	buildResp, err := this.dockerCli.ImageBuild(this.ctx, contextTar, types.ImageBuildOptions{
		Tags:       []string{tag},
		NoCache:    true,
		Dockerfile: "Dockerfile",
	})
	if err != nil {
		return fmt.Errorf("failed to build cs:go container: %w", err)
	}

	if _, err := io.Copy(ioutil.Discard, buildResp.Body); err != nil {
		return fmt.Errorf("error while reading build log: %w", err)
	}

	log.Trace().Msg("Finished building base image")

	return nil
}

func (this *UpdateWatcher) buildContainer(baseImage string, resultTag string, dockerfile string) error {
	log.Info().Msg("Building preinstalled image")

	contextTar, err := os.Open(this.buildContextFile)
	if err != nil {
		return fmt.Errorf("failed to open build context tar: %w", err)
	}

	buildResp, err := this.dockerCli.ImageBuild(this.ctx, contextTar, types.ImageBuildOptions{
		Tags:       []string{resultTag},
		NoCache:    true,
		Dockerfile: dockerfile,
		BuildArgs: map[string]*string{
			"BASE_IMAGE": &baseImage,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to build cs:go container: %w", err)
	}

	//buildOutput := ioutil.Discard
	buildOutput := os.Stdout
	if _, err := io.Copy(buildOutput, buildResp.Body); err != nil {
		return fmt.Errorf("error while reading build log: %w", err)
	}

	log.Trace().Msg("Finished building preinstalled image")

	return err
}

func (this *UpdateWatcher) getImageBuildid(tag string) (int, error) {
	logs, err := this.runScript("/usr/src/helper-installed-buildid.sh", tag)
	if err != nil {
		return 0, fmt.Errorf("faile to run script for getting installed CS:GO version: %w", err)
	}

	buildid, err := strconv.Atoi(logs[:len(logs)-1])
	if err != nil {
		return 0, fmt.Errorf("failed to parse buildid of newly build cs:go container")
	}

	return buildid, nil
}
