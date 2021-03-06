package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/cppforlife/go-semi-semantic/version"
)

const userPropertyName = "user"

//go:generate counterfeiter . Worker

type Worker interface {
	ActiveContainers() int
	ActiveVolumes() int
	BuildContainers() int

	Description() string
	Name() string
	ResourceTypes() []atc.WorkerResourceType
	Tags() atc.Tags
	Uptime() time.Duration
	IsOwnedByTeam() bool
	Ephemeral() bool
	IsVersionCompatible(lager.Logger, version.Version) bool
	Satisfies(lager.Logger, WorkerSpec) bool

	FindContainerByHandle(lager.Logger, int, string) (Container, bool, error)
	FindOrCreateContainer(
		context.Context,
		lager.Logger,
		ImageFetchingDelegate,
		db.ContainerOwner,
		db.ContainerMetadata,
		ContainerSpec,
		creds.VersionedResourceTypes,
	) (Container, error)

	FindVolumeForResourceCache(logger lager.Logger, resourceCache db.UsedResourceCache) (Volume, bool, error)
	FindVolumeForTaskCache(lager.Logger, int, int, string, string) (Volume, bool, error)

	CertsVolume(lager.Logger) (volume Volume, found bool, err error)
	LookupVolume(lager.Logger, string) (Volume, bool, error)
	CreateVolume(logger lager.Logger, spec VolumeSpec, teamID int, volumeType db.VolumeType) (Volume, error)

	GardenClient() garden.Client
}

type gardenWorker struct {
	gardenClient      garden.Client
	volumeClient      VolumeClient
	imageFactory      ImageFactory
	containerProvider ContainerProvider
	dbWorker          db.Worker
	buildContainers   int
}

// NewGardenWorker constructs a Worker using the gardenWorker runtime implementation and allows container and volume
// creation on a specific Garden worker.
// A Garden Worker is comprised of: db.Worker, garden Client, container provider, and a volume client
func NewGardenWorker(
	gardenClient garden.Client,
	containerProvider ContainerProvider,
	volumeClient VolumeClient,
	imageFactory ImageFactory,
	dbWorker db.Worker,
	numBuildContainers int,
	// TODO: numBuildContainers is only needed for placement strategy but this
	// method is called in ContainerProvider.FindOrCreateContainer as well and
	// hence we pass in 0 values for numBuildContainers everywhere.
) Worker {
	return &gardenWorker{
		gardenClient:      gardenClient,
		volumeClient:      volumeClient,
		imageFactory:      imageFactory,
		containerProvider: containerProvider,
		dbWorker:          dbWorker,
		buildContainers:   numBuildContainers,
	}
}

func (worker *gardenWorker) GardenClient() garden.Client {
	return worker.gardenClient
}

func (worker *gardenWorker) IsVersionCompatible(logger lager.Logger, comparedVersion version.Version) bool {
	workerVersion := worker.dbWorker.Version()
	logger = logger.Session("check-version", lager.Data{
		"want-worker-version": comparedVersion.String(),
		"have-worker-version": workerVersion,
	})

	if workerVersion == nil {
		logger.Info("empty-worker-version")
		return false
	}

	v, err := version.NewVersionFromString(*workerVersion)
	if err != nil {
		logger.Error("failed-to-parse-version", err)
		return false
	}

	switch v.Release.Compare(comparedVersion.Release) {
	case 0:
		return true
	case -1:
		return false
	default:
		if v.Release.Components[0].Compare(comparedVersion.Release.Components[0]) == 0 {
			return true
		}

		return false
	}
}

func (worker *gardenWorker) FindResourceTypeByPath(path string) (atc.WorkerResourceType, bool) {
	for _, rt := range worker.dbWorker.ResourceTypes() {
		if path == rt.Image {
			return rt, true
		}
	}

	return atc.WorkerResourceType{}, false
}

func (worker *gardenWorker) FindVolumeForResourceCache(logger lager.Logger, resourceCache db.UsedResourceCache) (Volume, bool, error) {
	return worker.volumeClient.FindVolumeForResourceCache(logger, resourceCache)
}

func (worker *gardenWorker) FindVolumeForTaskCache(logger lager.Logger, teamID int, jobID int, stepName string, path string) (Volume, bool, error) {
	return worker.volumeClient.FindVolumeForTaskCache(logger, teamID, jobID, stepName, path)
}

func (worker *gardenWorker) CertsVolume(logger lager.Logger) (Volume, bool, error) {
	return worker.volumeClient.FindOrCreateVolumeForResourceCerts(logger.Session("find-or-create"))
}

func (worker *gardenWorker) CreateVolume(logger lager.Logger, spec VolumeSpec, teamID int, volumeType db.VolumeType) (Volume, error) {
	return worker.volumeClient.CreateVolume(logger.Session("find-or-create"), spec, teamID, worker.dbWorker.Name(), volumeType)
}

func (worker *gardenWorker) LookupVolume(logger lager.Logger, handle string) (Volume, bool, error) {
	return worker.volumeClient.LookupVolume(logger, handle)
}

func (worker *gardenWorker) FindOrCreateContainer(
	ctx context.Context,
	logger lager.Logger,
	delegate ImageFetchingDelegate,
	owner db.ContainerOwner,
	metadata db.ContainerMetadata,
	containerSpec ContainerSpec,
	resourceTypes creds.VersionedResourceTypes,
) (Container, error) {

	image, err := worker.imageFactory.GetImage(
		logger,
		worker,
		worker.volumeClient,
		containerSpec.ImageSpec,
		containerSpec.TeamID,
		delegate,
		resourceTypes,
	)
	if err != nil {
		return nil, err
	}
	return worker.containerProvider.FindOrCreateContainer(
		ctx,
		logger,
		owner,
		delegate,
		metadata,
		containerSpec,
		image,
	)
}

func (worker *gardenWorker) FindContainerByHandle(logger lager.Logger, teamID int, handle string) (Container, bool, error) {
	return worker.containerProvider.FindCreatedContainerByHandle(logger, handle, teamID)
}

// TODO: are these required on the Worker object?
// does the caller already have the db.Worker available?
func (worker *gardenWorker) ActiveContainers() int {
	return worker.dbWorker.ActiveContainers()
}

func (worker *gardenWorker) ActiveVolumes() int {
	return worker.dbWorker.ActiveVolumes()
}

func (worker *gardenWorker) Name() string {
	return worker.dbWorker.Name()
}

func (worker *gardenWorker) ResourceTypes() []atc.WorkerResourceType {
	return worker.dbWorker.ResourceTypes()
}

func (worker *gardenWorker) Tags() atc.Tags {
	return worker.dbWorker.Tags()
}

func (worker *gardenWorker) Ephemeral() bool {
	return worker.dbWorker.Ephemeral()
}

func (worker *gardenWorker) BuildContainers() int {
	return worker.buildContainers
}

func (worker *gardenWorker) Satisfies(logger lager.Logger, spec WorkerSpec) bool {
	workerTeamID := worker.dbWorker.TeamID()
	workerResourceTypes := worker.dbWorker.ResourceTypes()

	if spec.TeamID != workerTeamID && workerTeamID != 0 {
		return false
	}

	if spec.ResourceType != "" {
		underlyingType := determineUnderlyingTypeName(spec.ResourceType, spec.ResourceTypes)

		matchedType := false
		for _, t := range workerResourceTypes {
			if t.Type == underlyingType {
				matchedType = true
				break
			}
		}

		if !matchedType {
			return false
		}
	}

	if spec.Platform != "" {
		if spec.Platform != worker.dbWorker.Platform() {
			return false
		}
	}

	if !worker.tagsMatch(spec.Tags) {
		return false
	}

	return true
}

func determineUnderlyingTypeName(typeName string, resourceTypes creds.VersionedResourceTypes) string {
	resourceTypesMap := make(map[string]creds.VersionedResourceType)
	for _, resourceType := range resourceTypes {
		resourceTypesMap[resourceType.Name] = resourceType
	}
	underlyingTypeName := typeName
	underlyingType, ok := resourceTypesMap[underlyingTypeName]
	for ok {
		underlyingTypeName = underlyingType.Type
		underlyingType, ok = resourceTypesMap[underlyingTypeName]
		delete(resourceTypesMap, underlyingTypeName)
	}
	return underlyingTypeName
}

func (worker *gardenWorker) Description() string {
	messages := []string{
		fmt.Sprintf("platform '%s'", worker.dbWorker.Platform()),
	}

	for _, tag := range worker.dbWorker.Tags() {
		messages = append(messages, fmt.Sprintf("tag '%s'", tag))
	}

	return strings.Join(messages, ", ")
}

func (worker *gardenWorker) IsOwnedByTeam() bool {
	return worker.dbWorker.TeamID() != 0
}

func (worker *gardenWorker) Uptime() time.Duration {
	return time.Since(time.Unix(worker.dbWorker.StartTime(), 0))
}

func (worker *gardenWorker) tagsMatch(tags []string) bool {
	workerTags := worker.dbWorker.Tags()
	if len(workerTags) > 0 && len(tags) == 0 {
		return false
	}

insert_coin:
	for _, stag := range tags {
		for _, wtag := range workerTags {
			if stag == wtag {
				continue insert_coin
			}
		}

		return false
	}

	return true
}
