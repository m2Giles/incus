package main

import (
	"io/ioutil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"

	log "github.com/lxc/lxd/shared/log15"
)

var containersCmd = APIEndpoint{
	Name: "containers",

	Get:  APIEndpointAction{Handler: containersGet},
	Post: APIEndpointAction{Handler: containersPost},
}

var containerCmd = APIEndpoint{
	Name: "containers/{name}",

	Get:    APIEndpointAction{Handler: containerGet},
	Put:    APIEndpointAction{Handler: containerPut},
	Delete: APIEndpointAction{Handler: containerDelete},
	Post:   APIEndpointAction{Handler: containerPost},
	Patch:  APIEndpointAction{Handler: containerPatch},
}

var containerStateCmd = APIEndpoint{
	Name: "containers/{name}/state",

	Get: APIEndpointAction{Handler: containerState},
	Put: APIEndpointAction{Handler: containerStatePut},
}

var containerFileCmd = APIEndpoint{
	Name: "containers/{name}/files",

	Get:    APIEndpointAction{Handler: containerFileHandler},
	Post:   APIEndpointAction{Handler: containerFileHandler},
	Delete: APIEndpointAction{Handler: containerFileHandler},
}

var containerSnapshotsCmd = APIEndpoint{
	Name: "containers/{name}/snapshots",

	Get:  APIEndpointAction{Handler: containerSnapshotsGet},
	Post: APIEndpointAction{Handler: containerSnapshotsPost},
}

var containerSnapshotCmd = APIEndpoint{
	Name: "containers/{name}/snapshots/{snapshotName}",

	Get:    APIEndpointAction{Handler: containerSnapshotHandler},
	Post:   APIEndpointAction{Handler: containerSnapshotHandler},
	Delete: APIEndpointAction{Handler: containerSnapshotHandler},
	Put:    APIEndpointAction{Handler: containerSnapshotHandler},
}

var containerConsoleCmd = APIEndpoint{
	Name: "containers/{name}/console",

	Get:    APIEndpointAction{Handler: containerConsoleLogGet},
	Post:   APIEndpointAction{Handler: containerConsolePost},
	Delete: APIEndpointAction{Handler: containerConsoleLogDelete},
}

var containerExecCmd = APIEndpoint{
	Name: "containers/{name}/exec",

	Post: APIEndpointAction{Handler: containerExecPost},
}

var containerMetadataCmd = APIEndpoint{
	Name: "containers/{name}/metadata",

	Get: APIEndpointAction{Handler: containerMetadataGet},
	Put: APIEndpointAction{Handler: containerMetadataPut},
}

var containerMetadataTemplatesCmd = APIEndpoint{
	Name: "containers/{name}/metadata/templates",

	Get:    APIEndpointAction{Handler: containerMetadataTemplatesGet},
	Post:   APIEndpointAction{Handler: containerMetadataTemplatesPostPut},
	Put:    APIEndpointAction{Handler: containerMetadataTemplatesPostPut},
	Delete: APIEndpointAction{Handler: containerMetadataTemplatesDelete},
}

var containerBackupsCmd = APIEndpoint{
	Name: "containers/{name}/backups",

	Get:  APIEndpointAction{Handler: containerBackupsGet},
	Post: APIEndpointAction{Handler: containerBackupsPost},
}

var containerBackupCmd = APIEndpoint{
	Name: "containers/{name}/backups/{backupName}",

	Get:    APIEndpointAction{Handler: containerBackupGet},
	Post:   APIEndpointAction{Handler: containerBackupPost},
	Delete: APIEndpointAction{Handler: containerBackupDelete},
}

var containerBackupExportCmd = APIEndpoint{
	Name: "containers/{name}/backups/{backupName}/export",

	Get: APIEndpointAction{Handler: containerBackupExportGet},
}

type containerAutostartList []container

func (slice containerAutostartList) Len() int {
	return len(slice)
}

func (slice containerAutostartList) Less(i, j int) bool {
	iOrder := slice[i].ExpandedConfig()["boot.autostart.priority"]
	jOrder := slice[j].ExpandedConfig()["boot.autostart.priority"]

	if iOrder != jOrder {
		iOrderInt, _ := strconv.Atoi(iOrder)
		jOrderInt, _ := strconv.Atoi(jOrder)
		return iOrderInt > jOrderInt
	}

	return slice[i].Name() < slice[j].Name()
}

func (slice containerAutostartList) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func containersRestart(s *state.State) error {
	// Get all the containers
	result, err := containerLoadNodeAll(s)
	if err != nil {
		return err
	}

	containers := []container{}

	for _, c := range result {
		containers = append(containers, c)
	}

	sort.Sort(containerAutostartList(containers))

	// Restart the containers
	for _, c := range containers {
		config := c.ExpandedConfig()
		lastState := config["volatile.last_state.power"]

		autoStart := config["boot.autostart"]
		autoStartDelay := config["boot.autostart.delay"]

		if shared.IsTrue(autoStart) || (autoStart == "" && lastState == "RUNNING") {
			if c.IsRunning() {
				continue
			}

			err = c.Start(false)
			if err != nil {
				logger.Errorf("Failed to start container '%s': %v", c.Name(), err)
			}

			autoStartDelayInt, err := strconv.Atoi(autoStartDelay)
			if err == nil {
				time.Sleep(time.Duration(autoStartDelayInt) * time.Second)
			}
		}
	}

	return nil
}

type containerStopList []container

func (slice containerStopList) Len() int {
	return len(slice)
}

func (slice containerStopList) Less(i, j int) bool {
	iOrder := slice[i].ExpandedConfig()["boot.stop.priority"]
	jOrder := slice[j].ExpandedConfig()["boot.stop.priority"]

	if iOrder != jOrder {
		iOrderInt, _ := strconv.Atoi(iOrder)
		jOrderInt, _ := strconv.Atoi(jOrder)
		return iOrderInt > jOrderInt // check this line (prob <)
	}

	return slice[i].Name() < slice[j].Name()
}

func (slice containerStopList) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func containersShutdown(s *state.State) error {
	var wg sync.WaitGroup

	dbAvailable := true

	// Get all the containers
	containers, err := containerLoadNodeAll(s)
	if err != nil {
		// Mark database as offline
		dbAvailable = false
		containers = []container{}

		// List all containers on disk
		files, err := ioutil.ReadDir(shared.VarPath("containers"))
		if err != nil {
			return err
		}

		for _, file := range files {
			project := "default"
			name := file.Name()
			if strings.Contains(name, "_") {
				fields := strings.Split(file.Name(), "_")
				project = fields[0]
				name = fields[1]
			}

			c, err := containerLXCLoad(s, db.ContainerArgs{
				Project: project,
				Name:    name,
				Config:  make(map[string]string),
			}, nil)
			if err != nil {
				return err
			}

			containers = append(containers, c)
		}
	}

	sort.Sort(containerStopList(containers))

	if dbAvailable {
		// Reset all container states
		err = s.Cluster.ContainersResetState()
		if err != nil {
			return err
		}
	}

	var lastPriority int = 0

	if len(containers) != 0 {
		lastPriority, _ = strconv.Atoi(containers[0].ExpandedConfig()["boot.stop.priority"])
	}

	for _, c := range containers {
		priority, _ := strconv.Atoi(c.ExpandedConfig()["boot.stop.priority"])

		// Enforce shutdown priority
		if priority != lastPriority {
			lastPriority = priority

			// Wait for containers with higher priority to finish
			wg.Wait()
		}

		// Record the current state
		lastState := c.State()

		// Stop the container
		if lastState != "BROKEN" && lastState != "STOPPED" {
			// Determinate how long to wait for the container to shutdown cleanly
			var timeoutSeconds int
			value, ok := c.ExpandedConfig()["boot.host_shutdown_timeout"]
			if ok {
				timeoutSeconds, _ = strconv.Atoi(value)
			} else {
				timeoutSeconds = 30
			}

			// Stop the container
			wg.Add(1)
			go func(c container, lastState string) {
				c.Shutdown(time.Second * time.Duration(timeoutSeconds))
				c.Stop(false)
				c.ConfigKeySet("volatile.last_state.power", lastState)

				wg.Done()
			}(c, lastState)
		} else {
			c.ConfigKeySet("volatile.last_state.power", lastState)
		}
	}
	wg.Wait()

	return nil
}

func containerDeleteSnapshots(s *state.State, project, cname string) error {
	results, err := s.Cluster.ContainerGetSnapshots(project, cname)
	if err != nil {
		return err
	}

	for _, sname := range results {
		sc, err := containerLoadByProjectAndName(s, project, sname)
		if err != nil {
			logger.Error(
				"containerDeleteSnapshots: Failed to load the snapshotcontainer",
				log.Ctx{"container": cname, "snapshot": sname})

			continue
		}

		if err := sc.Delete(); err != nil {
			logger.Error(
				"containerDeleteSnapshots: Failed to delete a snapshotcontainer",
				log.Ctx{"container": cname, "snapshot": sname, "err": err})
		}
	}

	return nil
}
