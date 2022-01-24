package test

import (
"context"
"fmt"
"github.com/docker/go-connections/nat"
"github.com/kurtosis-tech/container-engine-lib/lib/docker_manager"
"github.com/kurtosis-tech/container-engine-lib/lib/docker_manager/types"
"github.com/kurtosis-tech/kurtosis-core/launcher/api_container_launcher"
"github.com/kurtosis-tech/kurtosis-core/launcher/enclave_container_launcher"
"github.com/kurtosis-tech/kurtosis-engine-server/api/golang/kurtosis_engine_rpc_api_bindings"
"github.com/kurtosis-tech/kurtosis-engine-server/server/engine/enclave_manager/docker_network_allocator"
"github.com/kurtosis-tech/object-attributes-schema-lib/forever_constants"
"github.com/kurtosis-tech/object-attributes-schema-lib/schema"
"github.com/kurtosis-tech/stacktrace"
"github.com/sirupsen/logrus"
"io/ioutil"
"net"
"os"
"path"
"sort"
"strconv"
"strings"
"sync"
)

const (
	shouldFetchStoppedContainersWhenGettingEnclaveStatus = true

	// We set this to true in case there are any race conditions with a container starting as we're trying to stop the enclave
	shouldKillAlreadyStoppedContainersWhenStoppingEnclave = true

	shouldFetchStoppedContainersWhenDestroyingEnclave = true

	// API container port number string parsing constants
	hostMachinePortNumStrParsingBase = 10
	hostMachinePortNumStrParsingBits = 16

	allEnclavesDirname = "enclaves"

	apiContainerListenPortNumInsideNetwork = uint16(7443)

	// NOTE: It's very important that all directories created inside the engine data directory are created with 0777
	//  permissions, because:
	//  a) the engine data directory is bind-mounted on the Docker host machine
	//  b) the engine container, and pretty much every Docker container, runs as 'root'
	//  c) the Docker host machine will not be running as root
	//  d) For the host machine to be able to read & write files inside the engine data directory, it needs to be able
	//      to access the directories inside the engine data directory
	// The longterm fix to this is probably to:
	//  1) make the engine server data a Docker volume
	//  2) have the engine server expose the engine data directory to the Docker host machine via some filesystem-sharing
	//      server, like NFS or CIFS
	// This way, we preserve the host machine's ability to write to services as if they were local on the filesystem, while
	//  actually having the data live inside a Docker volume. This also sets the stage for Kurtosis-as-a-Service (where bind-mount
	//  the engine data dirpath would be impossible).
	engineDataSubdirectoryPerms = 0777

	// --------------------------- Old port parsing constants ------------------------------------
	// These are the old labels that the API container used to use before 2021-11-15 for declaring its port num protocol
	// We can get rid of this after 2022-05-15, when we're confident no users will be running API containers with the old label
	pre2021_11_15_apiContainerPortNumLabel    = "com.kurtosistech.api-container-port-number"
	pre2021_11_15_apiContainerPortNumBase     = 10
	pre2021_11_15_apiContainerPortNumUintBits = 16
	pre2021_11_15_apiContainerPortProtocol    = schema.PortProtocol_TCP

	// These are the old labels that the API container used to use before 2021-12-02 for declaring its port num protocol
	// We can get rid of this after 2022-06-02, when we're confident no users will be running API containers with the old label
	pre2021_12_02_apiContainerPortNumLabel    = "com.kurtosistech.port-number"
	pre2021_12_02_apiContainerPortNumBase     = 10
	pre2021_12_02_apiContainerPortNumUintBits = 16
	pre2021_12_02_apiContainerPortProtocol    = schema.PortProtocol_TCP
	// --------------------------- Old port parsing constants ------------------------------------

	enclavesCleaningPhaseTitle             = "enclaves"
	metadataAcquisitionTestsuitePhaseTitle = "metadata-acquiring testsuite containers"

	// Obviously yes
	shouldFetchStoppedContainersWhenDestroyingStoppedContainers = true
)

// Unfortunately, Docker doesn't have constants for the protocols it supports declared
var objAttrsSchemaPortProtosToDockerPortProtos = map[schema.PortProtocol]string{
	schema.PortProtocol_TCP:  "tcp",
	schema.PortProtocol_SCTP: "sctp",
	schema.PortProtcol_UDP:   "udp",
}

// Manages Kurtosis enclaves, and creates new ones in response to running tasks
type EnclaveManager struct {
	// We use Docker as our backing datastore, but it has tons of race conditions so we use this mutex to ensure
	//  enclave modifications are atomic
	mutex *sync.Mutex

	dockerManager *docker_manager.DockerManager

	objAttrsProvider schema.ObjectAttributesProvider

	dockerNetworkAllocator *docker_network_allocator.DockerNetworkAllocator

	// We need this so that when the engine container starts enclaves & containers, it can tell Docker where the enclave
	//  data directory is on the host machine os Docker can bind-mount it in
	engineDataDirpathOnHostMachine string

	engineDataDirpathOnEngineContainer string
}

func NewEnclaveManager(
	dockerManager *docker_manager.DockerManager,
	objectAttributesProvider schema.ObjectAttributesProvider,
	engineDataDirpathOnHostMachine string,
	engineDataDirpathOnEngineContainer string,
) *EnclaveManager {
	dockerNetworkAllocator := docker_network_allocator.NewDockerNetworkAllocator(dockerManager)
	return &EnclaveManager{
		mutex:                              &sync.Mutex{},
		dockerManager:                      dockerManager,
		objAttrsProvider:                   objectAttributesProvider,
		dockerNetworkAllocator:             dockerNetworkAllocator,
		engineDataDirpathOnHostMachine:     engineDataDirpathOnHostMachine,
		engineDataDirpathOnEngineContainer: engineDataDirpathOnEngineContainer,
	}
}

// It's a liiiitle weird that we return an EnclaveInfo object (which is a Protobuf object), but as of 2021-10-21 this class
//  is only used by the EngineServerService so we might as well return the object that EngineServerService wants
func (manager *EnclaveManager) CreateEnclave(
	setupCtx context.Context,
// If blank, will use the default
	apiContainerImageVersionTag string,
	apiContainerLogLevel logrus.Level,
// TODO put in coreApiVersion as a param here!
	enclaveId string,
	isPartitioningEnabled bool,
	shouldPublishAllPorts bool,
) (*kurtosis_engine_rpc_api_bindings.EnclaveInfo, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	teardownCtx := context.Background() // Separate context for tearing stuff down in case the input context is cancelled

	_, found, err := manager.getEnclaveNetwork(setupCtx, enclaveId)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred checking for networks with name '%v', which is necessary to ensure that our enclave doesn't exist yet", enclaveId)
	}
	if found {
		return nil, stacktrace.NewError("Cannot create enclave '%v' because an enclave with that name already exists", enclaveId)
	}

	_, allEnclavesDirpathOnEngineContainer := manager.getAllEnclavesDirpaths()
	if err := ensureDirpathExists(allEnclavesDirpathOnEngineContainer); err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred ensuring enclaves directory '%v' exists", allEnclavesDirpathOnEngineContainer)
	}

	enclaveDataDirpathOnHostMachine, enclaveDataDirpathOnEngineContainer := manager.getEnclaveDataDirpath(enclaveId)
	if _, err := os.Stat(enclaveDataDirpathOnEngineContainer); err == nil {
		return nil, stacktrace.NewError("Cannot create enclave '%v' because an enclave data directory already exists at '%v'", enclaveId, enclaveDataDirpathOnEngineContainer)
	}
	if err := ensureDirpathExists(enclaveDataDirpathOnEngineContainer); err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred ensuring enclave data directory '%v' exists", enclaveDataDirpathOnEngineContainer)
	}
	shouldDeleteEnclaveDataDir := true
	defer func() {
		if shouldDeleteEnclaveDataDir {
			if err := os.RemoveAll(enclaveDataDirpathOnEngineContainer); err != nil {
				// 'clean' will remove this dangling directories, so this is a warn rather than an error
				logrus.Warnf("Enclave creation didn't complete successfully so we tried to remove the enclave data directory '%v', but removing threw the following error; this directory will stay around until a clean is run", enclaveDataDirpathOnEngineContainer)
				fmt.Fprintln(logrus.StandardLogger().Out, err)
			}
		}
	}()

	enclaveObjAttrsProvider := manager.objAttrsProvider.ForEnclave(enclaveId)
	enclaveNetworkAttrs := enclaveObjAttrsProvider.ForEnclaveNetwork()
	enclaveNetworkName := enclaveNetworkAttrs.GetName()
	enclaveNetworkLabels := enclaveNetworkAttrs.GetLabels()

	logrus.Debugf("Creating Docker network for enclave '%v'...", enclaveId)
	networkId, networkIpAndMask, gatewayIp, freeIpAddrTracker, err := manager.dockerNetworkAllocator.CreateNewNetwork(
		setupCtx,
		enclaveNetworkName,
		enclaveNetworkLabels,
	)
	if err != nil {
		// TODO If the user Ctrl-C's while the CreateNetwork call is ongoing then the CreateNetwork will error saying
		//  that the Context was cancelled as expected, but *the Docker engine will still create the network*!!! We'll
		//  need to parse the log message for the string "context canceled" and, if found, do another search for
		//  networks with our network name and delete them
		return nil, stacktrace.Propagate(err, "An error occurred allocating a new network for enclave '%v'", enclaveId)
	}
	shouldDeleteNetwork := true
	defer func() {
		if shouldDeleteNetwork {
			if err := manager.dockerManager.RemoveNetwork(teardownCtx, networkId); err != nil {
				logrus.Errorf("Creating the enclave didn't complete successfully, so we tried to delete network '%v' that we created but an error was thrown:", networkId)
				fmt.Fprintln(logrus.StandardLogger().Out, err)
				logrus.Errorf("ACTION REQUIRED: You'll need to manually remove network with ID '%v'!!!!!!!", networkId)
			}
		}
	}()
	logrus.Debugf("Docker network '%v' created successfully with ID '%v' and subnet CIDR '%v'", enclaveId, networkId, networkIpAndMask.String())

	// We need to create the IP addresses for BOTH containers because the testsuite needs to know the IP of the API
	//  container which will only be started after the testsuite container
	apiContainerPrivateIpAddr, err := freeIpAddrTracker.GetFreeIpAddr()
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting an IP for the Kurtosis API container")
	}

	enclaveContainerLauncher := enclave_container_launcher.NewEnclaveContainerLauncher(
		manager.dockerManager,
		enclaveObjAttrsProvider,
		enclaveDataDirpathOnHostMachine,
	)

	apiContainerLauncher := api_container_launcher.NewApiContainerLauncher(
		enclaveContainerLauncher,
		manager.dockerManager,
	)
	var apiContainerId string
	var apiContainerPublicIpAddr net.IP
	var apiContainerPublicPort *enclave_container_launcher.EnclaveContainerPort
	var launchApiContainerErr error
	if apiContainerImageVersionTag == "" {
		apiContainerId, apiContainerPublicIpAddr, apiContainerPublicPort, launchApiContainerErr = apiContainerLauncher.LaunchWithDefaultVersion(
			setupCtx,
			apiContainerLogLevel,
			enclaveId,
			networkId,
			networkIpAndMask.String(),
			apiContainerListenPortNumInsideNetwork,
			gatewayIp,
			apiContainerPrivateIpAddr,
			isPartitioningEnabled,
			enclaveDataDirpathOnHostMachine,
		)
	} else {
		apiContainerId, apiContainerPublicIpAddr, apiContainerPublicPort, launchApiContainerErr = apiContainerLauncher.LaunchWithCustomVersion(
			setupCtx,
			apiContainerImageVersionTag,
			apiContainerLogLevel,
			enclaveId,
			networkId,
			networkIpAndMask.String(),
			apiContainerListenPortNumInsideNetwork,
			gatewayIp,
			apiContainerPrivateIpAddr,
			isPartitioningEnabled,
			enclaveDataDirpathOnHostMachine,
		)
	}
	if launchApiContainerErr != nil {
		return nil, stacktrace.Propagate(launchApiContainerErr, "An error occurred launching the API container")
	}
	shouldStopApiContainer := true
	defer func() {
		if shouldStopApiContainer {
			if err := manager.dockerManager.KillContainer(teardownCtx, apiContainerId); err != nil {
				logrus.Errorf("Creating the enclave didn't complete successfully, so we tried to kill the API container but an error was thrown:")
				fmt.Fprintln(logrus.StandardLogger().Out, err)
				logrus.Errorf("ACTION REQUIRED: You'll need to manually stop API container with ID '%v'", apiContainerId)
			}
		}
	}()

	result := &kurtosis_engine_rpc_api_bindings.EnclaveInfo{
		EnclaveId:          enclaveId,
		NetworkId:          networkId,
		NetworkCidr:        networkIpAndMask.String(),
		ContainersStatus:   kurtosis_engine_rpc_api_bindings.EnclaveContainersStatus_EnclaveContainersStatus_RUNNING,
		ApiContainerStatus: kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerStatus_EnclaveAPIContainerStatus_RUNNING,
		ApiContainerInfo: &kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerInfo{
			ContainerId:       apiContainerId,
			IpInsideEnclave:   apiContainerPrivateIpAddr.String(),
			PortInsideEnclave: uint32(apiContainerListenPortNumInsideNetwork),
		},
		ApiContainerHostMachineInfo: &kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerHostMachineInfo{
			IpOnHostMachine:   apiContainerPublicIpAddr.String(),
			PortOnHostMachine: uint32(apiContainerPublicPort.GetNumber()),
		},
		EnclaveDataDirpathOnHostMachine: enclaveDataDirpathOnHostMachine,
	}

	// Everything started successfully, so the responsibility of deleting the enclave is now transferred to the caller
	shouldDeleteEnclaveDataDir = false
	shouldDeleteNetwork = false
	shouldStopApiContainer = false
	return result, nil
}

// It's a liiiitle weird that we return an EnclaveInfo object (which is a Protobuf object), but as of 2021-10-21 this class
//  is only used by the EngineServerService so we might as well return the object that EngineServerService wants
func (manager *EnclaveManager) GetEnclaves(
	ctx context.Context,
) (map[string]*kurtosis_engine_rpc_api_bindings.EnclaveInfo, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	enclaves, err := manager.getEnclavesWithoutMutex(ctx)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the enclaves without the mutex")
	}
	return enclaves, nil
}

func (manager *EnclaveManager) StopEnclave(ctx context.Context, enclaveId string) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	if err := manager.stopEnclaveWithoutMutex(ctx, enclaveId); err != nil {
		return stacktrace.Propagate(err, "An error occurred stopping enclave '%v'", enclaveId)
	}
	return nil
}

// Destroys an enclave, deleting all objects associated with it in the container engine (containers, volumes, networks, etc.)
func (manager *EnclaveManager) DestroyEnclave(ctx context.Context, enclaveId string) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	if err := manager.destroyEnclaveWithoutMutex(ctx, enclaveId); err != nil {
		return stacktrace.Propagate(err, "An error occurred destroying the enclave without the mutex")
	}
	return nil
}

func (manager *EnclaveManager) Clean(ctx context.Context, shouldCleanAll bool) (map[string]bool, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	resultSuccessfullyRemovedArtifactsIds := map[string]map[string]bool{}

	// Map of cleaning_phase_title -> (successfully_destroyed_object_id, object_destruction_errors, clean_error)
	cleaningPhaseFunctions := map[string]func() ([]string, []error, error){
		enclavesCleaningPhaseTitle: func() ([]string, []error, error) {
			return manager.cleanEnclaves(ctx, shouldCleanAll)
		},
		metadataAcquisitionTestsuitePhaseTitle: func() ([]string, []error, error) {
			return manager.cleanMetadataAcquisitionTestsuites(ctx, shouldCleanAll)
		},
	}

	phaseErrors := map[string]error{}
	for phaseTitle, cleaningFunc := range cleaningPhaseFunctions {
		logrus.Infof("Cleaning %v...", phaseTitle)
		successfullyRemovedArtifactIds, removalErrors, err := cleaningFunc()
		// PRESSING ">>" ON THIS LINE ONLY INDENTS 4 CHARACTERS
		if err != nil {
			// BUT ">>" ON THIS LINE INDENTS 5
			phaseErrors[phaseTitle] = stacktrace.Propagate(
				// AND "<<" HERE ONLY DEINDENTS 3 CHARACTERS
				err,
				// SAME 3-DEINDENT PROBLEM HERE
				"An error occurred while calling the cleaning function",
			)
			continue
		// BUT DEINDENTING THIS DOES THE PROPER 4-SPACE DEINDENT
		}

		if len(successfullyRemovedArtifactIds) > 0 {

			artifactIDs := map[string]bool{}
			logrus.Infof("Successfully removed the following %v:", phaseTitle)
			sort.Strings(successfullyRemovedArtifactIds)
			for _, successfulArtifactId := range successfullyRemovedArtifactIds {
				artifactIDs[successfulArtifactId] = true
				fmt.Fprintln(logrus.StandardLogger().Out, successfulArtifactId)
			}
			resultSuccessfullyRemovedArtifactsIds[phaseTitle] = artifactIDs
		}

		if len(removalErrors) > 0 {
			allErrStrings := []string{}
			for idx, removalErr := range removalErrors {
				errStr := fmt.Sprintf(
					">>>>>>>>>>>>>>>>>> Error %v <<<<<<<<<<<<<<<<<<\n%v",
					idx,
					removalErr.Error(),
				)
				allErrStrings = append(allErrStrings, errStr)
			}



			logrus.Error f("Errors occurred removing the following %v:", phaseTitle)
			for _, err := range removalErrors {
				fmt.Fprintln(logrus.StandardLogger().Out, "")
				fmt.Fprintln(logrus.StandardLogger().Out, err.Error())
			}
			phasesWithErrors = append(phasesWithErrors, phaseTitle)

			newlineDelimitedErrorStrs := strings.Join(errorStrs)
			phaseErrors[phaseTitle] = stacktrace.NewError(
				"The following errors occurred during cleaning phase '%v':\n%v",
				phaseTitle,

			)
			continue
		}
		logrus.Infof("Successfully cleaned %v", phaseTitle)
	}

	if len(phasesWithErrors) > 0 {
		errorStr := "Errors occurred cleaning " + strings.Join(phasesWithErrors, ", ")
		return nil, stacktrace.NewError(errorStr)
	}

	return resultSuccessfullyRemovedArtifactsIds[enclavesCleaningPhaseTitle], nil
}

// ====================================================================================================
// 									   Private helper methods
// ====================================================================================================
// There is a 1:1 mapping between Docker network and enclave - no network, no enclave, and vice versa
// We therefore use this function to check for the existence of an enclave, as well as get network info about existing enclaves
func (manager *EnclaveManager) getEnclaveNetwork(ctx context.Context, enclaveId string) (*types.Network, bool, error) {
	matchingNetworks, err := manager.dockerManager.GetNetworksByName(ctx, enclaveId)
	if err != nil {
		return nil, false, stacktrace.Propagate(err, "An error occurred getting networks matching name '%v'", enclaveId)
	}
	numMatchingNetworks := len(matchingNetworks)
	logrus.Debugf("Found %v networks matching name '%v': %+v", numMatchingNetworks, enclaveId, matchingNetworks)
	if numMatchingNetworks > 1 {
		return nil, false, stacktrace.NewError(
			"Found %v networks matching name '%v' when we expected just one - this is likely a bug in Kurtosis!",
			numMatchingNetworks,
			enclaveId,
		)
	}
	if numMatchingNetworks == 0 {
		return nil, false, nil
	}
	network := matchingNetworks[0]
	return network, true, nil
}

func getEnclaveContainerInformation(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaveId string,
) (
	kurtosis_engine_rpc_api_bindings.EnclaveContainersStatus,
	kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerStatus,
	*kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerInfo,
	*kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerHostMachineInfo,
	error,
) {
	containers, err := getEnclaveContainers(ctx, dockerManager, enclaveId)
	if err != nil {
		return 0, 0, nil, nil, stacktrace.Propagate(err, "An error occurred getting the containers for enclave '%v'", enclaveId)
	}
	if len(containers) == 0 {
		return kurtosis_engine_rpc_api_bindings.EnclaveContainersStatus_EnclaveContainersStatus_EMPTY,
			kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerStatus_EnclaveAPIContainerStatus_NONEXISTENT,
			nil,
			nil,
			nil
	}

	resultContainersStatus := kurtosis_engine_rpc_api_bindings.EnclaveContainersStatus_EnclaveContainersStatus_STOPPED
	resultApiContainerStatus := kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerStatus_EnclaveAPIContainerStatus_NONEXISTENT
	var resultApiContainerInfo *kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerInfo = nil
	var resultApiContainerHostMachineInfo *kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerHostMachineInfo = nil
	for _, container := range containers {
		containerStatus := container.GetStatus()
		isContainerRunning := containerStatus == types.Running || containerStatus == types.Restarting
		if isContainerRunning {
			resultContainersStatus = kurtosis_engine_rpc_api_bindings.EnclaveContainersStatus_EnclaveContainersStatus_RUNNING
		}

		// Parse API container info, if it exists
		containerLabels := container.GetLabels()
		containerTypeLabelValue, found := containerLabels[forever_constants.ContainerTypeLabel]
		if found && containerTypeLabelValue == schema.ContainerTypeAPIContainer {
			if resultApiContainerInfo != nil {
				return 0, 0, nil, nil, stacktrace.NewError("Found a second API container inside the network; this should never happen!")
			}

			if isContainerRunning {
				resultApiContainerStatus = kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerStatus_EnclaveAPIContainerStatus_RUNNING
			} else {
				resultApiContainerStatus = kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerStatus_EnclaveAPIContainerStatus_STOPPED
			}

			apiContainerIpInsideNetwork, foundApiContainerIpLabel := containerLabels[schema.APIContainerIPLabel]
			if !foundApiContainerIpLabel {
				return 0, 0, nil, nil, stacktrace.NewError(
					"No label '%v' was found on the API container indicating its IP inside the network",
					schema.APIContainerIPLabel,
				)
			}

			apiContainerObjAttrPrivatePort, getPrivatePortErr := getApiContainerPrivatePortUsingAllKnownMethods(containerLabels)
			if getPrivatePortErr != nil {
				return 0, 0, nil, nil, stacktrace.Propagate(err, "An error occurred getting the API container private port")
			}

			resultApiContainerInfo = &kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerInfo{
				ContainerId:       container.GetId(),
				IpInsideEnclave:   apiContainerIpInsideNetwork,
				PortInsideEnclave: uint32(apiContainerObjAttrPrivatePort.GetNumber()),
			}

			// We only get host machine info if the container is running
			if isContainerRunning {
				apiContainerPrivatePortObjAttrProto := apiContainerObjAttrPrivatePort.GetProtocol()
				apiContainerPrivatePortDockerProto, foundDockerProto := objAttrsSchemaPortProtosToDockerPortProtos[apiContainerPrivatePortObjAttrProto]
				if !foundDockerProto {
					return 0, 0, nil, nil, stacktrace.NewError(
						"No Docker protocol was defined for obj attr proto '%v'; this is a bug in Kurtosis",
						apiContainerPrivatePortObjAttrProto,
					)
				}

				apiContainerPrivatePortNumStr := fmt.Sprintf("%v", apiContainerObjAttrPrivatePort.GetNumber())
				apiContainerDockerPrivatePort, createDockerPrivatePortErr := nat.NewPort(
					apiContainerPrivatePortDockerProto,
					apiContainerPrivatePortNumStr,
				)
				if createDockerPrivatePortErr != nil {
					return 0, 0, nil, nil, stacktrace.Propagate(
						createDockerPrivatePortErr,
						"An error occurred creating the API container Docker private port object from port number '%v' and protocol '%v', which is necessary for getting its host machine port bindings",
						apiContainerPrivatePortNumStr,
						apiContainerPrivatePortDockerProto,
					)
				}

				allApiContainerPublicPortBindings := container.GetHostPortBindings()
				apiContainerPublicPortBinding, foundApiContainerPublicPortBinding := allApiContainerPublicPortBindings[apiContainerDockerPrivatePort]
				if !foundApiContainerPublicPortBinding {
					return 0, 0, nil, nil, stacktrace.NewError(
						"No host machine port binding was found for API container Docker port '%v'; this is a bug in Kurtosis!",
						apiContainerDockerPrivatePort,
					)
				}

				apiContainerPublicPortNumStr := apiContainerPublicPortBinding.HostPort
				apiContainerPublicPortNumUint16, err := parseHostMachinePortNumStrToUint16(apiContainerPublicPortNumStr)
				if err != nil {
					return 0, 0, nil, nil, stacktrace.Propagate(err, "An error occurred converting the API container public port string '%v' to uint16", apiContainerPublicPortNumStr)
				}

				resultApiContainerHostMachineInfo = &kurtosis_engine_rpc_api_bindings.EnclaveAPIContainerHostMachineInfo{
					IpOnHostMachine:   apiContainerPublicPortBinding.HostIP,
					PortOnHostMachine: uint32(apiContainerPublicPortNumUint16),
				}
			}
		}
	}

	return resultContainersStatus, resultApiContainerStatus, resultApiContainerInfo, resultApiContainerHostMachineInfo, nil
}

func getApiContainerPrivatePortUsingAllKnownMethods(apiContainerLabels map[string]string) (*schema.PortSpec, error) {
	serializedPortSpecsStr, found := apiContainerLabels[schema.PortSpecsLabel]
	if found {
		portSpecs, err := schema.DeserializePortSpecs(serializedPortSpecsStr)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred deserializing API container port spec string '%v'", serializedPortSpecsStr)
		}
		port, found := portSpecs[schema.KurtosisInternalContainerGRPCPortID]
		if !found {
			return nil, stacktrace.NewError("Didn't find expected API container port ID '%v' in the port specs map", schema.KurtosisInternalContainerGRPCPortID)
		}
		return port, nil
	}

	pre2021_12_02Port, err := getApiContainerPrivatePortUsingPre2021_12_02Label(apiContainerLabels)
	if err == nil {
		return pre2021_12_02Port, nil
	} else {
		logrus.Debugf("An error occurred getting the API container private port num using the pre-2021-12-02 label: %v", err)
	}

	pre2021_11_15Port, err := getApiContainerPrivatePortUsingPre2021_11_15Label(apiContainerLabels)
	if err == nil {
		return pre2021_11_15Port, nil
	} else {
		logrus.Debugf("An error occurred getting the API container private port num using the pre-2021-11-15 label: %v", err)
	}

	return nil, stacktrace.NewError("Couldn't get the API container private port number using any of the known methods")
}

func getApiContainerPrivatePortUsingPre2021_11_15Label(containerLabels map[string]string) (*schema.PortSpec, error) {
	// We can get rid of this after 2022-05-15, when we're confident no users will be running API containers with this label
	portNumStr, found := containerLabels[pre2021_11_15_apiContainerPortNumLabel]
	if !found {
		return nil, stacktrace.NewError("Couldn't get API container private port using the pre-2021-11-15 label '%v' because it doesn't exist", pre2021_11_15_apiContainerPortNumLabel)
	}
	portNumUint64, err := strconv.ParseUint(portNumStr, pre2021_11_15_apiContainerPortNumBase, pre2021_11_15_apiContainerPortNumUintBits)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred parsing pre-2021-11-15 private port num string '%v' to a uint16", portNumStr)
	}
	portNumUint16 := uint16(portNumUint64) // Safe to do because we pass in the number of bits to the ParseUint call above
	result, err := schema.NewPortSpec(portNumUint16, pre2021_11_15_apiContainerPortProtocol)
	if err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred creating a new port spec using pre-2021-11-15 port num '%v' and protocol '%v'",
			portNumUint16,
			pre2021_11_15_apiContainerPortProtocol,
		)
	}
	return result, nil
}

func getApiContainerPrivatePortUsingPre2021_12_02Label(containerLabels map[string]string) (*schema.PortSpec, error) {
	// We can get rid of this after 2022-06-02, when we're confident no users will be running API containers with this label
	portNumStr, found := containerLabels[pre2021_12_02_apiContainerPortNumLabel]
	if !found {
		return nil, stacktrace.NewError("Couldn't get API container private port using the pre-2021-12-02 label '%v' because it doesn't exist", pre2021_12_02_apiContainerPortNumLabel)
	}
	portNumUint64, err := strconv.ParseUint(portNumStr, pre2021_12_02_apiContainerPortNumBase, pre2021_12_02_apiContainerPortNumUintBits)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred parsing pre-2021-12-02 private port num string '%v' to a uint16", portNumStr)
	}
	portNumUint16 := uint16(portNumUint64) // Safe to do because we pass in the number of bits to the ParseUint call above
	result, err := schema.NewPortSpec(portNumUint16, pre2021_12_02_apiContainerPortProtocol)
	if err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred creating a new port spec using pre-2021-12-02 port num '%v' and protocol '%v'",
			portNumUint16,
			pre2021_12_02_apiContainerPortProtocol,
		)
	}
	return result, nil
}

func getEnclaveContainers(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaveId string,
) ([]*types.Container, error) {
	searchLabels := map[string]string{
		schema.EnclaveIDContainerLabel: enclaveId,
	}
	containers, err := dockerManager.GetContainersByLabels(ctx, searchLabels, shouldFetchStoppedContainersWhenGettingEnclaveStatus)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the containers for enclave '%v' by labels '%+v'", enclaveId, searchLabels)
	}
	return containers, nil
}

func parseHostMachinePortNumStrToUint16(input string) (uint16, error) {
	portNumUint64, err := strconv.ParseUint(input, hostMachinePortNumStrParsingBase, hostMachinePortNumStrParsingBits)
	if err != nil {
		return 0, stacktrace.Propagate(
			err,
			"An error occurred parsing port number string '%v' to uint of base %v with %v bits",
			input,
			hostMachinePortNumStrParsingBase,
			hostMachinePortNumStrParsingBits,
		)
	}
	return uint16(portNumUint64), nil
}

// Both StopEnclave and DestroyEnclave need to be able to stop enclaves, but both have a mutex guard. Because Go mutexes
//  aren't reentrant, DestroyEnclave can't just call StopEnclave so we use this helper function
func (manager *EnclaveManager) stopEnclaveWithoutMutex(ctx context.Context, enclaveId string) error {
	_, found, err := manager.getEnclaveNetwork(ctx, enclaveId)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred checking for the existence of a network for enclave '%v'", enclaveId)
	}
	if !found {
		return stacktrace.NewError("No enclave with ID '%v' exists", enclaveId)
	}

	enclaveContainerSearchLabels := map[string]string{
		schema.EnclaveIDContainerLabel: enclaveId,
	}
	allEnclaveContainers, err := manager.dockerManager.GetContainersByLabels(ctx, enclaveContainerSearchLabels, shouldKillAlreadyStoppedContainersWhenStoppingEnclave)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting containers for enclave '%v'", enclaveId)
	}
	logrus.Debugf("Containers in enclave '%v' that will be killed: %+v", enclaveId, allEnclaveContainers)

	// TODO Parallelize for perf
	containerKillErrorStrs := []string{}
	for _, enclaveContainer := range allEnclaveContainers {
		containerId := enclaveContainer.GetId()
		containerName := enclaveContainer.GetName()
		if err := manager.dockerManager.KillContainer(ctx, containerId); err != nil {
			wrappedContainerKillErr := stacktrace.Propagate(
				err,
				"An error occurred killing container '%v' with ID '%v'",
				containerName,
				containerId,
			)
			containerKillErrorStrs = append(
				containerKillErrorStrs,
				wrappedContainerKillErr.Error(),
			)
		}
	}

	if len(containerKillErrorStrs) > 0 {
		errorStr := strings.Join(containerKillErrorStrs, "\n\n")
		return stacktrace.NewError(
			"One or more errors occurred killing the containers in enclave '%v':\n%v",
			enclaveId,
			errorStr,
		)
	}

	// If all the kills went off successfully, wait for all the containers we just killed to definitively exit
	//  before we return
	containerWaitErrorStrs := []string{}
	for _, enclaveContainer := range allEnclaveContainers {
		containerName := enclaveContainer.GetName()
		containerId := enclaveContainer.GetId()
		if _, err := manager.dockerManager.WaitForExit(ctx, containerId); err != nil {
			wrappedContainerWaitErr := stacktrace.Propagate(
				err,
				"An error occurred waiting for container '%v' with ID '%v' to exit after killing",
				containerName,
				containerId,
			)
			containerWaitErrorStrs = append(
				containerWaitErrorStrs,
				wrappedContainerWaitErr.Error(),
			)
		}
	}

	if len(containerWaitErrorStrs) > 0 {
		errorStr := strings.Join(containerWaitErrorStrs, "\n\n")
		return stacktrace.NewError(
			"One or more errors occurred waiting for containers in enclave '%v' to exit after killing, meaning we can't guarantee the enclave is completely stopped:\n%v",
			enclaveId,
			errorStr,
		)
	}

	return nil
}

// TODO This is copied from Kurt Core; merge these (likely into an EngineDataVolume object)!!!
func ensureDirpathExists(absoluteDirpath string) error {
	if _, statErr := os.Stat(absoluteDirpath); os.IsNotExist(statErr) {
		if mkdirErr := os.Mkdir(absoluteDirpath, engineDataSubdirectoryPerms); mkdirErr != nil {
			return stacktrace.Propagate(
				mkdirErr,
				"Directory '%v' in the engine data volume didn't exist, and an error occurred trying to create it",
				absoluteDirpath)
		}
	}
	// This is necessary because the os.Mkdir might not create the directory with the perms that we want due to the umask
	// Chmod is not affected by the umask, so this will guarantee we get a directory with the perms that we want
	// NOTE: This has the added benefit of, if this directory already exists (due to the user running an old engine from
	//  before 2021-11-16), we'll correct the perms
	if err := os.Chmod(absoluteDirpath, engineDataSubdirectoryPerms); err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred setting the permissions on directory '%v' to '%v'",
			absoluteDirpath,
			engineDataSubdirectoryPerms,
		)
	}
	return nil
}

func (manager *EnclaveManager) getAllEnclavesDirpaths() (onHostMachine string, onEngineContainer string) {
	onHostMachine = path.Join(
		manager.engineDataDirpathOnHostMachine,
		allEnclavesDirname,
	)
	onEngineContainer = path.Join(
		manager.engineDataDirpathOnEngineContainer,
		allEnclavesDirname,
	)
	return
}

func (manager *EnclaveManager) getEnclaveDataDirpath(enclaveId string) (onHostMachine string, onEngineContainer string) {
	allEnclavesOnHostMachine, allEnclavesOnEngineContainer := manager.getAllEnclavesDirpaths()
	onHostMachine = path.Join(
		allEnclavesOnHostMachine,
		enclaveId,
	)
	onEngineContainer = path.Join(
		allEnclavesOnEngineContainer,
		enclaveId,
	)
	return
}

func (manager *EnclaveManager) cleanEnclaves(ctx context.Context, shouldCleanAll bool) ([]string, []error, error) {
	enclaves, err := manager.getEnclavesWithoutMutex(ctx)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting enclaves to determine which need to be cleaned up")
	}

	enclaveIdsToDestroy := []string{}
	enclaveIdsToNotDestroy := []string{}
	for enclaveId, enclaveInfo := range enclaves {
		enclaveStatus := enclaveInfo.ContainersStatus
		if shouldCleanAll || enclaveStatus == kurtosis_engine_rpc_api_bindings.EnclaveContainersStatus_EnclaveContainersStatus_STOPPED {
			enclaveIdsToDestroy = append(enclaveIdsToDestroy, enclaveId)
		} else {
			enclaveIdsToNotDestroy = append(enclaveIdsToNotDestroy, enclaveId)
		}
	}

	successfullyDestroyedEnclaveIds := []string{}
	enclaveDestructionErrors := []error{}
	for _, enclaveId := range enclaveIdsToDestroy {
		if err := manager.destroyEnclaveWithoutMutex(ctx, enclaveId); err != nil {
			wrappedErr := stacktrace.Propagate(err, "An error occurred removing enclave '%v'", enclaveId)
			enclaveDestructionErrors = append(enclaveDestructionErrors, wrappedErr)
			continue
		}
		successfullyDestroyedEnclaveIds = append(successfullyDestroyedEnclaveIds, enclaveId)
	}

	//remove dangling folders if any
	err = manager.deleteDanglingDirectories(enclaveIdsToNotDestroy)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred while trying to delete dangling directories")
	}

	return successfullyDestroyedEnclaveIds, enclaveDestructionErrors, nil
}

func (manager *EnclaveManager) cleanContainers(ctx context.Context, searchLabels map[string]string, shouldKillRunningContainers bool) ([]string, []error, error) {
	matchingContainers, err := manager.dockerManager.GetContainersByLabels(
		ctx,
		searchLabels,
		shouldFetchStoppedContainersWhenDestroyingStoppedContainers,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting containers matching labels '%+v'", searchLabels)
	}

	containersToDestroy := []*types.Container{}
	for _, container := range matchingContainers {
		containerName := container.GetName()
		containerStatus := container.GetStatus()
		if shouldKillRunningContainers {
			containersToDestroy = append(containersToDestroy, container)
			continue
		}

		isRunning, err := isContainerRunning(containerStatus)
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred determining if container '%v' with status '%v' is running", containerName, containerStatus)
		}
		if !isRunning {
			containersToDestroy = append(containersToDestroy, container)
		}
	}

	successfullyDestroyedContainerNames := []string{}
	removeContainerErrors := []error{}
	for _, container := range containersToDestroy {
		containerId := container.GetId()
		containerName := container.GetName()
		if err := manager.dockerManager.RemoveContainer(ctx, containerId); err != nil {
			wrappedErr := stacktrace.Propagate(err, "An error occurred removing stopped container '%v'", containerName)
			removeContainerErrors = append(removeContainerErrors, wrappedErr)
			continue
		}
		successfullyDestroyedContainerNames = append(successfullyDestroyedContainerNames, containerName)
	}

	return successfullyDestroyedContainerNames, removeContainerErrors, nil
}

func isContainerRunning(status types.ContainerStatus) (bool, error) {
	switch status {
	case types.Running, types.Restarting:
		return true, nil
	case types.Paused, types.Removing, types.Dead, types.Created, types.Exited:
		return false, nil
	default:
		return false, stacktrace.NewError("Unrecognized container status '%v'; this is a bug in Kurtosis", status)

	}
}

// NOTE: We no longer have Kurtosis testsuites, so this can be removed after 2022-05-15 when we're confident no user will have metadata-acquiring testsuites in their Kurtosis engine anymore
func (manager *EnclaveManager) cleanMetadataAcquisitionTestsuites(ctx context.Context, shouldKillRunningContainers bool) ([]string, []error, error) {
	metadataAcquisitionTestsuiteLabels := map[string]string{
		forever_constants.ContainerTypeLabel: schema.ContainerTypeTestsuiteContainer,
		schema.TestsuiteTypeLabelKey:         schema.TestsuiteTypeLabelValue_MetadataAcquisition,
	}
	successfullyDestroyedContainerNames, containerDestructionErrors, err := manager.cleanContainers(ctx, metadataAcquisitionTestsuiteLabels, shouldKillRunningContainers)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred cleaning metadata-acquisition testsuite containers")
	}
	return successfullyDestroyedContainerNames, containerDestructionErrors, nil
}

func (manager *EnclaveManager) getEnclavesWithoutMutex(
	ctx context.Context,
) (map[string]*kurtosis_engine_rpc_api_bindings.EnclaveInfo, error) {
	// TODO this is janky; we need a better way to find enclave networks!!! Ideally, we shouldn't actually know the labbel keys or values here
	kurtosisNetworkLabels := map[string]string{
		forever_constants.AppIDLabel: forever_constants.AppIDValue,
	}

	networks, err := manager.dockerManager.GetNetworksByLabels(ctx, kurtosisNetworkLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting Kurtosis networks")
	}
	result := map[string]*kurtosis_engine_rpc_api_bindings.EnclaveInfo{}
	for _, network := range networks {
		enclaveId := network.GetName()
		// Container retrieval requires an extra call to the Docker engine per enclave, so therefore could be expensive
		//  if you have a LOT of enclaves. Maybe we want to make the getting of enclave container information be a separate
		//  engine server endpoint??
		containersStatus, apiContainerStatus, apiContainerInfo, apiContainerHostMachineInfo, err := getEnclaveContainerInformation(ctx, manager.dockerManager, enclaveId)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting information about the containers in enclave '%v'", enclaveId)
		}

		enclaveDataDirpathOnHostMachine, _ := manager.getEnclaveDataDirpath(enclaveId)
		enclaveInfo := &kurtosis_engine_rpc_api_bindings.EnclaveInfo{
			EnclaveId:                       enclaveId,
			NetworkId:                       network.GetId(),
			NetworkCidr:                     network.GetIpAndMask().String(),
			ContainersStatus:                containersStatus,
			ApiContainerStatus:              apiContainerStatus,
			ApiContainerInfo:                apiContainerInfo,
			ApiContainerHostMachineInfo:     apiContainerHostMachineInfo,
			EnclaveDataDirpathOnHostMachine: enclaveDataDirpathOnHostMachine,
		}
		result[enclaveId] = enclaveInfo
	}
	return result, nil
}

// Destroys an enclave, deleting all objects associated with it in the container engine (containers, volumes, networks, etc.)
func (manager *EnclaveManager) destroyEnclaveWithoutMutex(ctx context.Context, enclaveId string) error {
	enclaveNetwork, found, err := manager.getEnclaveNetwork(ctx, enclaveId)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred checking for a network for enclave '%v'", enclaveId)
	}
	if !found {
		return stacktrace.NewError("Cannot destroy enclave '%v' because no enclave with that ID exists", enclaveId)
	}

	if err := manager.stopEnclaveWithoutMutex(ctx, enclaveId); err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred stopping enclave with ID '%v', which is a prerequisite for destroying the enclave",
			enclaveId,
		)
	}

	// First, delete all enclave containers
	enclaveContainersSearchLabels := map[string]string{
		schema.EnclaveIDContainerLabel: enclaveId,
	}
	allEnclaveContainers, err := manager.dockerManager.GetContainersByLabels(
		ctx,
		enclaveContainersSearchLabels,
		shouldFetchStoppedContainersWhenDestroyingEnclave,
	)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the containers in enclave '%v'", enclaveId)
	}
	removeContainerErrorStrs := []string{}
	for _, container := range allEnclaveContainers {
		containerName := container.GetName()
		containerId := container.GetId()
		if err := manager.dockerManager.RemoveContainer(ctx, containerId); err != nil {
			wrappedErr := stacktrace.Propagate(
				err,
				"An error occurred removing container '%v' with ID '%v'",
				containerName,
				containerId,
			)
			removeContainerErrorStrs = append(
				removeContainerErrorStrs,
				wrappedErr.Error(),
			)
		}
	}
	if len(removeContainerErrorStrs) > 0 {
		return stacktrace.NewError(
			"An error occurred removing one or more containers in enclave '%v':\n%v",
			enclaveId,
			strings.Join(
				removeContainerErrorStrs,
				"\n\n",
			),
		)
	}

	// Next, remove the enclave data dir (if it exists)
	_, enclaveDataDirpathOnEngineContainer := manager.getEnclaveDataDirpath(enclaveId)
	if _, statErr := os.Stat(enclaveDataDirpathOnEngineContainer); !os.IsNotExist(statErr) {
		if removeErr := os.RemoveAll(enclaveDataDirpathOnEngineContainer); removeErr != nil {
			return stacktrace.Propagate(removeErr, "An error occurred removing enclave data dir '%v' on engine container", enclaveDataDirpathOnEngineContainer)
		}
	}

	// Finally, remove the network
	if err := manager.dockerManager.RemoveNetwork(ctx, enclaveNetwork.GetId()); err != nil {
		return stacktrace.Propagate(err, "An error occurred removing the network for enclave '%v'", enclaveId)
	}

	return nil
}

// Gets rid of dangling folders
func (manager *EnclaveManager) deleteDanglingDirectories(enclaveIdsToNotDestroy []string) error {
	_, allEnclavesDirpathOnEngineContainer := manager.getAllEnclavesDirpaths()
	fileInfos, err := ioutil.ReadDir(allEnclavesDirpathOnEngineContainer)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while reading the directory '%v' ", allEnclavesDirpathOnEngineContainer)
	}

	enclaves := map[string]bool{}
	for _, enclaveId := range enclaveIdsToNotDestroy {
		enclaves[enclaveId] = true
	}

	for _, fileInfo := range fileInfos {
		_, ok := enclaves[fileInfo.Name()]
		if fileInfo.IsDir() && !ok {
			folderPath := path.Join(allEnclavesDirpathOnEngineContainer, fileInfo.Name())
			if removeErr := os.RemoveAll(folderPath); removeErr != nil {
				return stacktrace.Propagate(removeErr, "An error occurred removing the data dir '%v' on engine container", folderPath)
			}
		}
	}

	return nil
}
