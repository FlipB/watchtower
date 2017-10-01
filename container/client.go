package container

import (
	"fmt"
	"io/ioutil"
	"time"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"golang.org/x/net/context"
)

const (
	defaultStopSignal = "SIGTERM"
)

// A Filter is a prototype for a function that can be used to filter the
// results from a call to the ListContainers() method on the Client.
type Filter func(Container) bool

// A Client is the interface through which watchtower interacts with the
// Docker API.
type Client interface {
	ListContainers(Filter) ([]Container, Services, error)
	StopContainer(Container, time.Duration) error
	StartContainer(Container) error
	RenameContainer(Container, string) error
	IsContainerStale(Container) (bool, error)
	IsServiceStale(Service) (bool, error)
	UpdateServiceImage(Service) error
	RemoveImage(Container) error
}

// NewClient returns a new Client instance which can be used to interact with
// the Docker API.
// The client reads its configuration from the following environment variables:
//  * DOCKER_HOST			the docker-engine host to send api requests to
//  * DOCKER_TLS_VERIFY		whether to verify tls certificates
//  * DOCKER_API_VERSION	the minimum docker api version to work with
func NewClient(pullImages bool) Client {
	cli, err := dockerclient.NewEnvClient()

	if err != nil {
		log.Fatalf("Error instantiating Docker client: %s", err)
	}

	return dockerClient{api: cli, pullImages: pullImages}
}

type dockerClient struct {
	api        *dockerclient.Client
	pullImages bool
}

func (client dockerClient) ListContainers(fn Filter) ([]Container, Services, error) {
	cs := []Container{}
	svcs := Services{}
	bg := context.Background()

	log.Debug("Retrieving services")

	services, err := client.api.ServiceList(bg, types.ServiceListOptions{})
	if (err != nil) {
		return nil, nil, err
	}

	for _, service := range services {
		name := service.Spec.Name
		id := service.ID
		image := service.Spec.TaskTemplate.ContainerSpec.Image

		imageInfo, _, err := client.api.ImageInspectWithRaw(bg, image)
		if err != nil {
			return nil, nil, err
		}

		s := Service{ name: name, id: id, imageInfo: &imageInfo, imageName: image }
		svcs.Add(s)
		log.Infof("Added service %s", name)
	}

	// service containers have label com.docker.swarm.service.id.

	log.Debug("Retrieving running containers")

	runningContainers, err := client.api.ContainerList(
		bg,
		types.ContainerListOptions{})
	if err != nil {
		return nil, nil, err
	}

	for _, runningContainer := range runningContainers {
		containerInfo, err := client.api.ContainerInspect(bg, runningContainer.ID)
		if err != nil {
			return nil, nil, err
		}

		imageInfo, _, err := client.api.ImageInspectWithRaw(bg, containerInfo.Image)
		if err != nil {
			return nil, nil, err
		}

		c := Container{containerInfo: &containerInfo, imageInfo: &imageInfo}
		if !fn(c) {
			log.Infof("Filtered out container id %s with image %s", runningContainer.ID, runningContainer.Image)
			continue;
		}

		containerServiceID := runningContainer.Labels["com.docker.swarm.service.id"];
		if (containerServiceID != "") {
			s, err := svcs.Service(containerServiceID)
			if (err != nil) {
				return nil, nil, err
			}
			log.Infof("Adding service container id %s to service %s", runningContainer.ID, s.name)
			s.AddContainer(c)
			continue;
		}

		cs = append(cs, c)
	}

	return cs, svcs, nil
}

func (client dockerClient) StopContainer(c Container, timeout time.Duration) error {
	bg := context.Background()
	signal := c.StopSignal()
	if signal == "" {
		signal = defaultStopSignal
	}

	log.Infof("Stopping %s (%s) with %s", c.Name(), c.ID(), signal)

	if err := client.api.ContainerKill(bg, c.ID(), signal); err != nil {
		return err
	}

	// Wait for container to exit, but proceed anyway after the timeout elapses
	client.waitForStop(c, timeout)

	if c.containerInfo.HostConfig.AutoRemove {
		log.Debugf("AutoRemove container %s, skipping ContainerRemove call.", c.ID())
	} else {
		log.Debugf("Removing container %s", c.ID())

		if err := client.api.ContainerRemove(bg, c.ID(), types.ContainerRemoveOptions{Force: true, RemoveVolumes: false}); err != nil {
			return err
		}
	}

	// Wait for container to be removed. In this case an error is a good thing
	if err := client.waitForStop(c, timeout); err == nil {
		return fmt.Errorf("Container %s (%s) could not be removed", c.Name(), c.ID())
	}

	return nil
}

func (client dockerClient) StartContainer(c Container) error {
	bg := context.Background()
	config := c.runtimeConfig()
	hostConfig := c.hostConfig()
	networkConfig := &network.NetworkingConfig{EndpointsConfig: c.containerInfo.NetworkSettings.Networks}
	// simpleNetworkConfig is a networkConfig with only 1 network.
	// see: https://github.com/docker/docker/issues/29265
	simpleNetworkConfig := func() *network.NetworkingConfig {
		oneEndpoint := make(map[string]*network.EndpointSettings)
		for k, v := range networkConfig.EndpointsConfig {
			oneEndpoint[k] = v
			// we only need 1
			break
		}
		return &network.NetworkingConfig{EndpointsConfig: oneEndpoint}
	}()

	name := c.Name()

	log.Infof("Creating %s", name)
	creation, err := client.api.ContainerCreate(bg, config, hostConfig, simpleNetworkConfig, name)
	if err != nil {
		return err
	}

	if !(hostConfig.NetworkMode.IsHost()) {

		for k := range simpleNetworkConfig.EndpointsConfig {
			err = client.api.NetworkDisconnect(bg, k, creation.ID, true)
			if err != nil {
				return err
			}
		}

		for k, v := range networkConfig.EndpointsConfig {
			err = client.api.NetworkConnect(bg, k, creation.ID, v)
			if err != nil {
				return err
			}
		}

	}

	log.Debugf("Starting container %s (%s)", name, creation.ID)

	err = client.api.ContainerStart(bg, creation.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
	}

	return nil

}

func (client dockerClient) RenameContainer(c Container, newName string) error {
	bg := context.Background()
	log.Debugf("Renaming container %s (%s) to %s", c.Name(), c.ID(), newName)
	return client.api.ContainerRename(bg, c.ID(), newName)
}

func (client dockerClient) IsContainerStale(c Container) (bool, error) {
	bg := context.Background()
	oldImageInfo := c.imageInfo
	imageName := c.ImageName()

	separatorIndex := strings.Index(imageName, "@")
	if (separatorIndex > -1) {
		// there's an @ in image name, this is probably a stack deployed container where image includes full tag and sha256 hash.
		// let's strip it!
		log.Infof("Container Stale check stripped image hash.")
		imageName = imageName[:separatorIndex]
	}

	if client.pullImages {
		log.Debugf("Pulling %s for %s", imageName, c.Name())

		var opts types.ImagePullOptions // ImagePullOptions can take a RegistryAuth arg to authenticate against a private registry
		auth, err := EncodedAuth(imageName)
		if err != nil {
			log.Debugf("Error loading authentication credentials %s", err)
			return false, err
		} else if auth == "" {
			log.Debugf("No authentication credentials found for %s", imageName)
			opts = types.ImagePullOptions{} // empty/no auth credentials
		} else {
			opts = types.ImagePullOptions{RegistryAuth: auth, PrivilegeFunc: DefaultAuthHandler}
		}

		response, err := client.api.ImagePull(bg, imageName, opts)
		if err != nil {
			log.Debugf("Error pulling image %s, %s", imageName, err)
			return false, err
		}
		defer response.Close()

		// the pull request will be aborted prematurely unless the response is read
		_, err = ioutil.ReadAll(response)
	}

	newImageInfo, _, err := client.api.ImageInspectWithRaw(bg, imageName)
	if err != nil {
		return false, err
	}

	if newImageInfo.ID != oldImageInfo.ID {
		log.Infof("Found new %s image (%s)", imageName, newImageInfo.ID)
		return true, nil
	} else {
		log.Debugf("No new images found for %s", c.Name())
	}

	return false, nil
}




func (client dockerClient) IsServiceStale(s Service) (bool, error) {
	bg := context.Background()
	oldImageInfo := s.imageInfo
	imageName := s.imageName

	log.Infof("In IS SERVICE STALE CHECK - about to pull images")
	log.Infof("ImageName: %s, ImageInfo.ID: %s", imageName, oldImageInfo.ID)

	separatorIndex := strings.Index(imageName, "@")
	if (separatorIndex > -1) {
		// there's an @ in image name, this is probably a stack deployed container where image includes full tag and sha256 hash.
		// let's strip it!
		log.Infof("Container Stale check stripped image hash.")
		imageName = imageName[:separatorIndex]
	}

	if client.pullImages {
		log.Debugf("Pulling %s for service %s", imageName, s.name)

		var opts types.ImagePullOptions // ImagePullOptions can take a RegistryAuth arg to authenticate against a private registry
		auth, err := EncodedAuth(imageName)
		if err != nil {
			log.Debugf("Error loading authentication credentials %s", err)
			return false, err
		} else if auth == "" {
			log.Debugf("No authentication credentials found for %s", imageName)
			opts = types.ImagePullOptions{} // empty/no auth credentials
		} else {
			log.Debugf("Using authentication credentials for %s", imageName)
			opts = types.ImagePullOptions{RegistryAuth: auth, PrivilegeFunc: DefaultAuthHandler}
		}

		response, err := client.api.ImagePull(bg, imageName, opts)
		if err != nil {
			log.Infof("Error pulling image %s, %s", imageName, err)
			return false, err
		}
		defer response.Close()

		// the pull request will be aborted prematurely unless the response is read
		_, err = ioutil.ReadAll(response)
		if (err != nil) {
			log.Infof("Error reading repsonse: %s", err.Error())
		}
	}

	newImageInfo, _, err := client.api.ImageInspectWithRaw(bg, imageName)
	if err != nil {
		return false, err
	}

	log.Infof("About to compare images")
	log.Infof("%s == %s", oldImageInfo.ID, newImageInfo.ID)

	if newImageInfo.ID != oldImageInfo.ID {
		log.Infof("Found new image %s (%s) for service %s", imageName, newImageInfo.ID, s.name)
		s.newImageInfo = &newImageInfo
		return true, nil
	} else {
		log.Debugf("No new images found for service %s", s.name)
	}

	return false, nil
}



func (client dockerClient) UpdateServiceImage(svc Service) error {
	bg := context.Background()

	service, _, err := client.api.ServiceInspectWithRaw(bg, svc.id)
	if (err != nil) {
		return err;
	}

	var updateOpts types.ServiceUpdateOptions
	
	var currentVersion = service.Version

	var currentSpec = service.Spec
	log.Infof("Service %s current image is %s", svc.name, currentSpec.TaskTemplate.ContainerSpec.Image)
	var newSpec = currentSpec
	newSpec.TaskTemplate.ContainerSpec.Image = svc.newImageInfo.ID;

	log.Infof("Service %s new image is %s", svc.name, newSpec.TaskTemplate.ContainerSpec.Image)

	response, err := client.api.ServiceUpdate(bg, svc.id, currentVersion, newSpec, updateOpts)
	if (err != nil) {
		return err
	}
	
	for _, warn := range response.Warnings {
		log.Infoln("UpdateServiceImage: " + warn)
	}

	return nil;
}


func (client dockerClient) RemoveImage(c Container) error {
	imageID := c.ImageID()
	log.Infof("Removing image %s", imageID)
	_, err := client.api.ImageRemove(context.Background(), imageID, types.ImageRemoveOptions{Force: true})
	return err
}

func (client dockerClient) waitForStop(c Container, waitTime time.Duration) error {
	bg := context.Background()
	timeout := time.After(waitTime)

	for {
		select {
		case <-timeout:
			return nil
		default:
			if ci, err := client.api.ContainerInspect(bg, c.ID()); err != nil {
				return err
			} else if !ci.State.Running {
				return nil
			}
		}

		time.Sleep(1 * time.Second)
	}
}
