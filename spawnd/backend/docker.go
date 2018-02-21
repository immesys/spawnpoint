package backend

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/SoftwareDefinedBuildings/spawnpoint/service"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
	"github.com/pkg/errors"
)

const defaultSpawnpointImage = "jhkolb/spawnpoint:amd64"

type Docker struct {
	Alias     string
	bw2Router string
	client    *docker.Client
}

func NewDocker(alias, bw2Router string) (*Docker, error) {
	client, err := docker.NewEnvClient()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to initialize docker client")
	}

	return &Docker{
		Alias:     alias,
		bw2Router: bw2Router,
		client:    client,
	}, nil
}

func (dkr *Docker) StartService(ctx context.Context, svcConfig *service.Configuration) (string, error) {
	baseImage := svcConfig.BaseImage
	if len(baseImage) == 0 {
		svcConfig.BaseImage = defaultSpawnpointImage
	}
	imageName, err := dkr.buildImage(ctx, svcConfig)
	if err != nil {
		return "", errors.Wrap(err, "Failed to build service Docker container")
	}

	envVars := []string{
		"BW2_DEFAULT_ENTITY=/srv/spawnpoint/entity.key",
		"BW2_AGENT=" + dkr.bw2Router,
	}
	containerConfig := &container.Config{
		Image:        imageName,
		Cmd:          svcConfig.Run,
		WorkingDir:   "/srv/spawnpoint",
		Env:          envVars,
		AttachStderr: true,
		AttachStdout: true,
	}

	hostConfig := &container.HostConfig{
		NetworkMode: container.NetworkMode("bridge"),
	}
	if svcConfig.UseHostNet {
		hostConfig.NetworkMode = container.NetworkMode("host")
	}

	containerName := fmt.Sprintf("%s_%s", dkr.Alias, svcConfig.Name)
	createdResult, err := dkr.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, containerName)
	if err != nil {
		return "", errors.Wrap(err, "Failed to create Docker container")
	}
	if err = dkr.client.ContainerStart(ctx, createdResult.ID, types.ContainerStartOptions{}); err != nil {
		return "", errors.Wrap(err, "Failed to start Docker container")
	}

	return createdResult.ID, nil
}

func (dkr *Docker) RestartService(ctx context.Context, id string) error {
	if err := dkr.client.ContainerRestart(ctx, id, nil); err != nil {
		return errors.Wrap(err, "Could not restart Docker container")
	}
	return nil
}

func (dkr *Docker) StopService(ctx context.Context, id string) error {
	if err := dkr.client.ContainerStop(ctx, id, nil); err != nil {
		return errors.Wrap(err, "Could not stop Docker container")
	}
	return nil
}

func (dkr *Docker) RemoveService(ctx context.Context, id string) error {
	if err := dkr.client.ContainerRemove(ctx, id, types.ContainerRemoveOptions{}); err != nil {
		return errors.Wrap(err, "Could not remove Docker container")
	}
	return nil
}

func (dkr *Docker) TailService(ctx context.Context, id string, log bool) (<-chan string, <-chan error) {
	msgChan := make(chan string, 20)
	errChan := make(chan error, 1)

	hijackResp, err := dkr.client.ContainerAttach(ctx, id, types.ContainerAttachOptions{
		Logs:   log,
		Stream: true,
		Stderr: true,
		Stdout: true,
	})
	if err != nil {
		close(msgChan)
		errChan <- errors.Wrap(err, "Failed to attach to Docker container")
		return msgChan, errChan
	}

	go func() {
		defer hijackResp.Close()
		for {
			msg, err := hijackResp.Reader.ReadString('\n')
			if err != nil {
				close(msgChan)
				if err != io.EOF {
					errChan <- errors.Wrap(err, "Failed to read container log message")
				}
				return
			}
			msgChan <- msg
		}
	}()

	return msgChan, errChan
}

func (dkr *Docker) MonitorService(ctx context.Context, id string) (<-chan Event, <-chan error) {
	transformedEvChan := make(chan Event, 20)
	transformedErrChan := make(chan error, 1)

	evChan, errChan := dkr.client.Events(ctx, types.EventsOptions{
		Filters: filters.NewArgs(filters.Arg("container", id)),
	})
	// Check for an error right away
	select {
	case err := <-errChan:
		close(transformedEvChan)
		transformedErrChan <- errors.Wrap(err,
			fmt.Sprintf("Failed to initialize event monitor for container %s", id))
	default:
	}

	go func() {
		for {
			select {
			case event := <-evChan:
				switch event.Action {
				case "die":
					transformedEvChan <- Die
				default:
				}
			case err := <-errChan:
				close(transformedEvChan)
				transformedErrChan <- errors.Wrap(err,
					fmt.Sprintf("Failed to retrieve log entry for container %s", id))
			}
		}
	}()

	return transformedEvChan, transformedErrChan
}

func (dkr *Docker) buildImage(ctx context.Context, svcConfig *service.Configuration) (string, error) {
	buildCtxt, err := generateBuildContext(svcConfig)
	if err != nil {
		return "", errors.Wrap(err, "Failed to generate Docker build context")
	}

	imgName := "spawnpoint_" + svcConfig.Name
	_, err = dkr.client.ImageBuild(ctx, buildCtxt, types.ImageBuildOptions{
		Tags:        []string{imgName},
		NoCache:     true,
		Context:     buildCtxt,
		Dockerfile:  "dockerfile",
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		return "", errors.Wrap(err, "Daemon failed to build image")
	}
	return imgName, nil
}

func generateDockerFile(config *service.Configuration) (*[]byte, error) {
	var dkrFileBuf bytes.Buffer
	dkrFileBuf.WriteString(fmt.Sprintf("FROM %s\n", config.BaseImage))
	if len(config.Source) > 0 {
		sourceparts := strings.SplitN(config.Source, "+", 2)
		switch sourceparts[0] {
		case "git":
			dkrFileBuf.WriteString(fmt.Sprintf("RUN git clone %s /srv/spawnpoint\n", sourceparts[1]))
		default:
			return nil, fmt.Errorf("Unkonwn source type: %s", config.Source)
		}
	}
	dkrFileBuf.WriteString("WORKDIR /srv/spawnpoint\n")
	dkrFileBuf.WriteString("COPY entity.key entity.key\n")
	for _, includedFile := range config.IncludedFiles[:len(config.IncludedFiles)-1] {
		dkrFileBuf.WriteString(fmt.Sprintf("COPY %s %s\n", includedFile, includedFile))
	}
	for _, includedDir := range config.IncludedDirectories {
		baseName := filepath.Base(includedDir)
		dkrFileBuf.WriteString(fmt.Sprintf("COPY %s %s\n", baseName, baseName))
	}

	for _, buildCmd := range config.Build {
		dkrFileBuf.WriteString(fmt.Sprintf("RUN %s\n", buildCmd))
	}

	contents := dkrFileBuf.Bytes()
	return &contents, nil
}

func generateBuildContext(config *service.Configuration) (io.Reader, error) {
	var buildCtxtBuffer bytes.Buffer
	tarWriter := tar.NewWriter(&buildCtxtBuffer)

	// Add Bosswave entity to the build context
	entity, err := base64.StdEncoding.DecodeString(config.BW2Entity)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to decode BW2 entity")
	}
	if err = tarWriter.WriteHeader(&tar.Header{
		Name: "entity.key",
		Size: int64(len(entity)),
	}); err != nil {
		return nil, errors.Wrap(err, "Failed to write entity file tar header")
	}
	if _, err = tarWriter.Write(entity); err != nil {
		return nil, errors.Wrap(err, "Failed to write entity file to tar")
	}

	// Add synthetic dockerfile to the build context
	dkrFileContents, err := generateDockerFile(config)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to generate dockerfile contents")
	}
	if err = tarWriter.WriteHeader(&tar.Header{
		Name: "dockerfile",
		Size: int64(len(*dkrFileContents)),
	}); err != nil {
		return nil, errors.Wrap(err, "Failed to write Dockerfile tar header")
	}
	if _, err = tarWriter.Write(*dkrFileContents); err != nil {
		return nil, errors.Wrap(err, "Failed to write Dockerfile to tar")
	}

	// Add any included files or directories to build context
	if len(config.IncludedFiles) > 0 {
		encoding := config.IncludedFiles[len(config.IncludedFiles)-1]
		decodedFiles, err := base64.StdEncoding.DecodeString(encoding)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to decode included files")
		}
		decodedFilesBuf := bytes.NewBuffer(decodedFiles)
		tarReader := tar.NewReader(decodedFilesBuf)
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, errors.Wrap(err, "Failed to read included files archive")
			}
			tarWriter.WriteHeader(header)
			io.Copy(tarWriter, tarReader)
		}
	}

	return &buildCtxtBuffer, nil
}