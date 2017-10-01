package container


import (
	"fmt"

	"github.com/docker/docker/api/types"
)

/*
// NewService sss
func NewService() {
	return &Service{
	}
}
*/

// Services is a collection of Service structs
type Services []Service

// Service returns a service matching the id
func (svcs Services) Service(id string) (*Service, error) {
	for _, s := range svcs {
		if (s.id == id) {
			return &s, nil;
		}
	}
	return nil, fmt.Errorf("no service with id %s found", id)
}

// Add a service to Services collection
func (svcs *Services) Add(svc Service) {
	var services []Service
	services = []Service(*svcs)
	services = append(services, svc)
	*svcs = Services(services)
}

// Service sss
type Service struct {
	Stale bool


	id				string
	name			string
	containers		[]Container
	imageInfo		*types.ImageInspect
	imageName		string
	newImageInfo	*types.ImageInspect
}

// AddContainer add a running container to a service.
func (s *Service) AddContainer(c Container) {
	s.containers = append(s.containers, c);
}

func (s Service) Name() string {
	return s.name
}

func (s Service) Containers() []Container {
	return s.containers
}