package commands

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"github.com/docker-slim/docker-slim/master/builder"
	"github.com/docker-slim/docker-slim/master/config"
	"github.com/docker-slim/docker-slim/master/docker/dockerclient"
	"github.com/docker-slim/docker-slim/master/inspectors/container"
	"github.com/docker-slim/docker-slim/master/inspectors/container/probes/http"
	"github.com/docker-slim/docker-slim/master/inspectors/image"
	"github.com/docker-slim/docker-slim/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/dustin/go-humanize"
)

func OnBuild(doDebug bool,
	statePath string,
	clientConfig *config.DockerClient,
	imageRef string,
	customImageTag string,
	doHttpProbe bool,
	httpProbeCmds []config.HttpProbeCmd,
	doRmFileArtifacts bool,
	doShowContainerLogs bool,
	imageOverrides map[string]bool,
	overrides *config.ContainerOverrides,
	volumeMounts map[string]config.VolumeMount,
	excludePaths map[string]bool,
	includePaths map[string]bool,
	continueAfter *config.ContinueAfter) {

	fmt.Printf("docker-slim: [build] image=%v http-probe=%v remove-file-artifacts=%v image-overrides=%+v entrypoint=%+v (%v) cmd=%+v (%v) workdir='%v' env=%+v expose=%+v\n",
		imageRef, doHttpProbe, doRmFileArtifacts,
		imageOverrides,
		overrides.Entrypoint, overrides.ClearEntrypoint, overrides.Cmd, overrides.ClearCmd,
		overrides.Workdir, overrides.Env, overrides.ExposedPorts)

	client := dockerclient.New(clientConfig)

	imageInspector, err := image.NewInspector(client, imageRef)
	utils.FailOn(err)

	if imageInspector.NoImage() {
		fmt.Println("docker-slim: [build] target image not found -", imageRef)
		return
	}

	log.Info("docker-slim: inspecting 'fat' image metadata...")
	err = imageInspector.Inspect()
	utils.FailOn(err)

	localVolumePath, artifactLocation := utils.PrepareSlimDirs(statePath, imageInspector.ImageInfo.ID)
	imageInspector.ArtifactLocation = artifactLocation

	log.Infof("docker-slim: [%v] 'fat' image size => %v (%v)\n",
		imageInspector.ImageInfo.ID,
		imageInspector.ImageInfo.VirtualSize,
		humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)))

	log.Info("docker-slim: processing 'fat' image info...")
	err = imageInspector.ProcessCollectedData()
	utils.FailOn(err)

	containerInspector, err := container.NewInspector(client,
		imageInspector,
		localVolumePath,
		overrides,
		doShowContainerLogs,
		volumeMounts,
		excludePaths,
		includePaths,
		doDebug)
	utils.FailOn(err)

	log.Info("docker-slim: starting instrumented 'fat' container...")
	err = containerInspector.RunContainer()
	utils.FailOn(err)

	log.Info("docker-slim: watching container monitor...")

	if "probe" == continueAfter.Mode {
		doHttpProbe = true
	}

	if doHttpProbe {
		probe, err := http.NewCustomProbe(containerInspector, httpProbeCmds)
		utils.FailOn(err)
		probe.Start()
		continueAfter.ContinueChan = probe.DoneChan()
	}

	switch continueAfter.Mode {
	case "enter":
		fmt.Println("docker-slim: press <enter> when you are done using the container...")
		creader := bufio.NewReader(os.Stdin)
		_, _, _ = creader.ReadLine()
	case "signal":
		fmt.Println("docker-slim: send SIGUSR1 when you are done using the container...")
		<-continueAfter.ContinueChan
		fmt.Println("docker-slim: got SIGUSR1...")
	case "timeout":
		fmt.Printf("docker-slim: waiting for the target container (%v seconds)...\n", int(continueAfter.Timeout))
		<-time.After(time.Second * continueAfter.Timeout)
		fmt.Printf("docker-slim: done waiting for the target container...")
	case "probe":
		fmt.Println("docker-slim: waiting for the HTTP probe to finish...")
		<-continueAfter.ContinueChan
		fmt.Println("docker-slim: HTTP probe is done...")
	default:
		utils.Fail("unknown continue-after mode")
	}

	containerInspector.FinishMonitoring()

	log.Info("docker-slim: shutting down 'fat' container...")
	err = containerInspector.ShutdownContainer()
	utils.WarnOn(err)

	if !containerInspector.HasCollectedData() {
		imageInspector.ShowFatImageDockerInstructions()
		fmt.Printf("docker-slim: [build] no data collected (no minified image generated) - done. (version: %v)\n", utils.CurrentVersion())
		return
	}

	log.Info("docker-slim: processing instrumented 'fat' container info...")
	err = containerInspector.ProcessCollectedData()
	utils.FailOn(err)

	if customImageTag == "" {
		customImageTag = imageInspector.SlimImageRepo
	}

	log.Info("docker-slim: building 'slim' image...")
	builder, err := builder.NewImageBuilder(client,
		customImageTag,
		imageInspector.ImageInfo,
		artifactLocation,
		imageOverrides,
		overrides)
	utils.FailOn(err)

	if !builder.HasData {
		log.Info("docker-slim: WARNING - no data artifacts")
	}

	err = builder.Build()
	utils.FailOn(err)

	log.Infoln("docker-slim: created new image:", builder.RepoName, "( has data artifacts:", builder.HasData, ")")

	if doRmFileArtifacts {
		log.Info("docker-slim: removing temporary artifacts...")
		err = utils.RemoveArtifacts(artifactLocation) //TODO: remove only the "files" subdirectory
		utils.WarnOn(err)
	}

	fmt.Println("docker-slim: [build] done.")
}
