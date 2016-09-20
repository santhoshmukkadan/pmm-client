/*
	Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package pmm

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/percona/kardianos-service"
	"github.com/percona/pmm/proto"
	"github.com/percona/pmm/proto/config"
)

// AddMySQLQueries add mysql instance to QAN.
func (a *Admin) AddMySQLQueries(info map[string]string) error {
	// Check if we have already this service on Consul.
	consulSvc, err := a.getConsulService("mysql:queries", a.ServiceName)
	if err != nil {
		return err
	}
	if consulSvc != nil {
		return ErrDuplicate
	}

	if err := a.checkGlobalDuplicateService("mysql:queries", a.ServiceName); err != nil {
		return err
	}

	// Now check if there are any mysql:queries services.
	consulSvc, err = a.getConsulService("mysql:queries", "")
	if err != nil {
		return err
	}

	// Register agent if config file does not exist.
	if !FileExists(agentConfigFile) {
		if err := a.registerAgent(); err != nil {
			return err
		}
	}

	agentID, err := getAgentID()
	if err != nil {
		return err
	}
	qanOSInstance, err := a.getQanOSInstance(agentID)
	if err == errOrphanedAgent {
		// If agent is orphaned, let's reinstall it.
		if err := a.registerAgent(); err != nil {
			return err
		}
		// Get new agent id.
		agentID, err = getAgentID()
		if err != nil {
			return err
		}
		// Get os instance again.
		qanOSInstance, err = a.getQanOSInstance(agentID)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	// Don't install service if we have already another "mysql:queries".
	// 1 agent handles multiple MySQL instances for QAN.
	var port uint
	if consulSvc == nil {
		if a.ServicePort > 0 {
			// The port is user defined.
			port, err = a.choosePort(a.ServicePort, true)
		} else {
			// Choose first port available starting the given default one.
			port, err = a.choosePort(42001, false)
		}
		if err != nil {
			return err
		}

		// Install and start service via platform service manager.
		// We have to run agent before adding it to QAN.
		svcConfig := &service.Config{
			Name:        fmt.Sprintf("pmm-mysql-queries-%d", port),
			DisplayName: "PMM Query Analytics agent",
			Description: "PMM Query Analytics agent",
			Executable:  fmt.Sprintf("%s/bin/percona-qan-agent", agentBaseDir),
			Arguments: []string{fmt.Sprintf("-listen=127.0.0.1:%d", port), "-basedir", agentBaseDir,
				"-pid-file", "\"\""},
		}
		if err := installService(svcConfig); err != nil {
			return err
		}
	} else {
		port = uint(consulSvc.Port)
		// Ensure qan-agent is started if service exists, otherwise it won't be enabled for QAN.
		if err := startService(fmt.Sprintf("pmm-mysql-queries-%d", port)); err != nil {
			return err
		}
	}

	// Add new MySQL instance to QAN.
	in := proto.Instance{
		Subsystem:  "mysql",
		ParentUUID: qanOSInstance.ParentUUID,
		Name:       a.ServiceName,
		DSN:        info["safe_dsn"],
		Distro:     info["distro"],
		Version:    info["version"],
	}
	inBytes, _ := json.Marshal(in)
	url := a.qanapi.URL(a.serverUrl, qanAPIBasePath, "instances")
	resp, content, err := a.qanapi.Post(url, inBytes)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusConflict:
		// instance already exists based on Name
	default:
		return a.qanapi.Error("POST", url, resp.StatusCode, http.StatusCreated, content)
	}

	// The URI of the new instance is reported in the Location header, fetch it to get UUID assigned.
	// Do not call the returned URL as QAN API returns an invalid one.
	var bytes []byte
	t := strings.Split(resp.Header.Get("Location"), "/")
	url = a.qanapi.URL(url, t[len(t)-1])
	resp, bytes, err = a.qanapi.Get(url)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return a.qanapi.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &in); err != nil {
		return err
	}

	// Start QAN by associating MySQL instance with agent.
	qanConfig := map[string]string{
		"UUID":        in.UUID,
		"CollectFrom": info["query_source"],
		"DSN":         info["dsn"],
	}
	if err := a.manageQAN(agentID, "StartTool", "", qanConfig); err != nil {
		return err
	}

	tags := []string{fmt.Sprintf("alias_%s", a.ServiceName)}
	// For existing service, we append a new alias_ tag.
	if consulSvc != nil {
		tags = append(consulSvc.Tags, tags...)
	}

	// Add or update service to Consul.
	serviceID := fmt.Sprintf("mysql:queries-%d", port)
	srv := consul.AgentService{
		ID:      serviceID,
		Service: "mysql:queries",
		Tags:    tags,
		Port:    int(port),
	}
	reg := consul.CatalogRegistration{
		Node:    a.Config.ClientName,
		Address: a.Config.ClientAddress,
		Service: &srv,
	}
	if _, err := a.consulapi.Catalog().Register(&reg, nil); err != nil {
		return err
	}

	// Add info to Consul KV.
	d := &consul.KVPair{Key: fmt.Sprintf("%s/%s/%s/dsn", a.Config.ClientName, serviceID, a.ServiceName),
		Value: []byte(info["safe_dsn"])}
	a.consulapi.KV().Put(d, nil)
	d = &consul.KVPair{Key: fmt.Sprintf("%s/%s/%s/query_source", a.Config.ClientName, serviceID, a.ServiceName),
		Value: []byte(info["query_source"])}
	a.consulapi.KV().Put(d, nil)
	d = &consul.KVPair{Key: fmt.Sprintf("%s/%s/%s/qan_mysql_uuid", a.Config.ClientName, serviceID, a.ServiceName),
		Value: []byte(in.UUID)}
	a.consulapi.KV().Put(d, nil)

	return nil
}

// RemoveMySQLQueries remove mysql instance from QAN.
func (a *Admin) RemoveMySQLQueries() error {
	// Check if we have this service on Consul.
	consulSvc, err := a.getConsulService("mysql:queries", a.ServiceName)
	if err != nil {
		return err
	}
	if consulSvc == nil {
		return ErrNoService
	}

	// Ensure qan-agent is started, otherwise it will be an error to stop QAN.
	if err := startService(fmt.Sprintf("pmm-mysql-queries-%d", consulSvc.Port)); err != nil {
		return err
	}

	// Get UUID of MySQL instance the agent is monitoring from KV.
	key := fmt.Sprintf("%s/%s/%s/qan_mysql_uuid", a.Config.ClientName, consulSvc.ID, a.ServiceName)
	data, _, err := a.consulapi.KV().Get(key, nil)
	if err != nil {
		return err
	}
	mysqlUUID := string(data.Value)

	// Stop QAN for this MySQL instance on the local agent.
	agentID, err := getAgentID()
	if err != nil {
		return err
	}
	if err := a.manageQAN(agentID, "StopTool", mysqlUUID, nil); err != nil {
		return err
	}

	// Remove MySQL instance from QAN.
	url := a.qanapi.URL(a.serverUrl, qanAPIBasePath, "instances", mysqlUUID)
	resp, content, err := a.qanapi.Delete(url)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusNoContent:
	default:
		return a.qanapi.Error("DELETE", url, resp.StatusCode, http.StatusNoContent, content)
	}

	prefix := fmt.Sprintf("%s/%s/%s/", a.Config.ClientName, consulSvc.ID, a.ServiceName)
	a.consulapi.KV().DeleteTree(prefix, nil)

	// Remove queries service from Consul only if we have only 1 tag alias_ (the instance in question).
	var tags []string
	for _, tag := range consulSvc.Tags {
		if strings.HasPrefix(tag, "alias_") {
			if tag != fmt.Sprintf("alias_%s", a.ServiceName) {
				tags = append(tags, tag)
			}
		}
	}
	if len(tags) == 0 {
		// Remove service from Consul.
		dereg := consul.CatalogDeregistration{
			Node:      a.Config.ClientName,
			ServiceID: consulSvc.ID,
		}
		if _, err := a.consulapi.Catalog().Deregister(&dereg, nil); err != nil {
			return err
		}

		// Stop and uninstall service.
		if err := uninstallService(fmt.Sprintf("pmm-mysql-queries-%d", consulSvc.Port)); err != nil {
			return err
		}
	} else {
		// Remove tag from service.
		consulSvc.Tags = tags
		reg := consul.CatalogRegistration{
			Node:    a.Config.ClientName,
			Address: a.Config.ClientAddress,
			Service: consulSvc,
		}
		if _, err := a.consulapi.Catalog().Register(&reg, nil); err != nil {
			return err
		}
	}

	return nil
}

// getQanOSInstance get os instance from QAN API that the local agent is associated with.
func (a *Admin) getQanOSInstance(agentID string) (proto.Instance, error) {
	var in proto.Instance
	url := a.qanapi.URL(a.serverUrl, qanAPIBasePath, "instances", agentID)
	resp, bytes, err := a.qanapi.Get(url)
	if err != nil {
		return in, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// No os instance on QAN API - orphaned agent installation.
		return in, errOrphanedAgent
	}
	if resp.StatusCode != http.StatusOK {
		return in, a.qanapi.Error("GET", url, resp.StatusCode, http.StatusOK, bytes)
	}
	if err := json.Unmarshal(bytes, &in); err != nil {
		return in, err
	}

	return in, nil
}

// startQAN call QAN API to start agent.
func (a *Admin) manageQAN(agentID, cmdName, UUID string, config map[string]string) error {
	var data []byte
	if cmdName == "StartTool" {
		data, _ = json.Marshal(config)
	} else if cmdName == "StopTool" {
		data = []byte(UUID)
	}
	cmd := proto.Cmd{
		User:    fmt.Sprintf("pmm-admin@%s", a.qanapi.Hostname()),
		Service: "qan",
		Cmd:     cmdName,
		Data:    data,
	}
	cmdBytes, _ := json.Marshal(cmd)

	// Send the command to the API which relays it to the agent, then relays the agent's reply back to here.
	url := a.qanapi.URL(a.serverUrl, qanAPIBasePath, "agents", agentID, "cmd")

	// It takes a few seconds for agent to connect to QAN API once it is started via service manager.
	// QAN API fails to start/stop unconnected agent for QAN, so we retry the request when getting 404 response.
RetryLoop:
	for i := 0; i < 10; i++ {
		resp, content, err := a.qanapi.Put(url, cmdBytes)
		if err != nil {
			return err
		}
		switch resp.StatusCode {
		case http.StatusNotFound:
			time.Sleep(time.Second)
			continue RetryLoop
		case http.StatusOK:
			return nil
		}
		return a.qanapi.Error("PUT", url, resp.StatusCode, http.StatusOK, content)
	}

	return fmt.Errorf("timeout 10s waiting on agent to connect to API.")
}

// getAgentID read QAN agent ID from its config file.
func getAgentID() (string, error) {
	jsonData, err := ioutil.ReadFile(agentConfigFile)
	if err != nil {
		return "", err
	}

	config := &config.Agent{}
	if err := json.Unmarshal(jsonData, &config); err != nil {
		return "", err
	}

	return config.UUID, nil
}

// registerAgent Register agent on QAN API using agent installer.
func (a *Admin) registerAgent() error {
	// Remove agent dirs to ensure clean installation. Using full paths to avoid unexpected removals.
	os.RemoveAll("/usr/local/percona/qan-agent/config")
	os.RemoveAll("/usr/local/percona/qan-agent/data")
	os.RemoveAll("/usr/local/percona/qan-agent/instance")

	path := fmt.Sprintf("%s/bin/percona-qan-agent-installer", agentBaseDir)
	args := []string{"-basedir", agentBaseDir, "-mysql=false"}
	if a.Config.ServerSSL {
		args = append(args, "-use-ssl")
	}
	if a.Config.ServerInsecureSSL {
		args = append(args, "-use-insecure-ssl")
	}
	if a.Config.ServerPassword != "" {
		args = append(args, fmt.Sprintf("-server-user=%s", a.Config.ServerUser),
			fmt.Sprintf("-server-pass=%s", a.Config.ServerPassword))
	}
	args = append(args, fmt.Sprintf("%s/%s", a.serverUrl, qanAPIBasePath))
	if _, err := exec.Command(path, args...).Output(); err != nil {
		return fmt.Errorf("problem with agent registration on QAN API: %s", err)
	}
	return nil
}