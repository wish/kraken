// Copyright (c) 2016-2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package cmd

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/uber/kraken/agent/agentserver"
	"github.com/uber/kraken/build-index/tagclient"
	"github.com/uber/kraken/core"
	"github.com/uber/kraken/lib/dockerregistry/transfer"
	"github.com/uber/kraken/lib/store"
	"github.com/uber/kraken/lib/torrent/networkevent"
	"github.com/uber/kraken/lib/torrent/scheduler"
	"github.com/uber/kraken/metrics"
	"github.com/uber/kraken/nginx"
	"github.com/uber/kraken/utils/configutil"
	"github.com/uber/kraken/utils/log"
	"github.com/uber/kraken/utils/netutil"

	"github.com/uber-go/tally"
)

// Flags defines agent CLI flags.
type Flags struct {
	PeerIP            string
	PeerPort          int
	AgentServerPort   int
	AgentRegistryPort int
	ConfigFile        string
	Zone              string
	KrakenCluster     string
	SecretsFile       string
}

// ParseFlags parses agent CLI flags.
func ParseFlags() *Flags {
	var flags Flags
	flag.StringVar(
		&flags.PeerIP, "peer-ip", "", "ip which peer will announce itself as")
	flag.IntVar(
		&flags.PeerPort, "peer-port", 0, "port which peer will announce itself as")
	flag.IntVar(
		&flags.AgentServerPort, "agent-server-port", 0, "port which agent server listens on")
	flag.IntVar(
		&flags.AgentRegistryPort, "agent-registry-port", 0, "port which agent registry listens on")
	flag.StringVar(
		&flags.ConfigFile, "config", "", "configuration file path")
	flag.StringVar(
		&flags.Zone, "zone", "", "zone/datacenter name")
	flag.StringVar(
		&flags.KrakenCluster, "cluster", "", "cluster name (e.g. prod01-zone1)")
	flag.StringVar(
		&flags.SecretsFile, "secrets", "", "path to a secrets YAML file to load into configuration")
	flag.Parse()
	return &flags
}

// Run runs the agent.
func Run(flags *Flags) {
	if flags.PeerPort == 0 {
		panic("must specify non-zero peer port")
	}
	if flags.AgentServerPort == 0 {
		panic("must specify non-zero agent server port")
	}
	if flags.AgentRegistryPort == 0 {
		panic("must specify non-zero agent registry port")
	}
	var config Config
	if err := configutil.Load(flags.ConfigFile, &config); err != nil {
		panic(err)
	}
	if flags.SecretsFile != "" {
		if err := configutil.Load(flags.SecretsFile, &config); err != nil {
			panic(err)
		}
	}

	zlog := log.ConfigureLogger(config.ZapLogging)
	defer zlog.Sync()

	stats, closer, err := metrics.New(config.Metrics, flags.KrakenCluster)
	if err != nil {
		log.Fatalf("Failed to init metrics: %s", err)
	}
	defer closer.Close()

	go metrics.EmitVersion(stats)

	if flags.PeerIP == "" {
		localIP, err := netutil.GetLocalIP()
		if err != nil {
			log.Fatalf("Error getting local ip: %s", err)
		}
		flags.PeerIP = localIP
	}

	pctx, err := core.NewPeerContext(
		config.PeerIDFactory, flags.Zone, flags.KrakenCluster, flags.PeerIP, flags.PeerPort, false)
	if err != nil {
		log.Fatalf("Failed to create peer context: %s", err)
	}

	cads, err := store.NewCADownloadStore(config.CADownloadStore, stats)
	if err != nil {
		log.Fatalf("Failed to create local store: %s", err)
	}

	netevents, err := networkevent.NewProducer(config.NetworkEvent)
	if err != nil {
		log.Fatalf("Failed to create network event producer: %s", err)
	}

	trackers, err := config.Tracker.Build()
	if err != nil {
		log.Fatalf("Error building tracker upstream: %s", err)
	}
	go trackers.Monitor(nil)

	tls, err := config.TLS.BuildClient()
	if err != nil {
		log.Fatalf("Error building client tls config: %s", err)
	}

	sched, err := scheduler.NewAgentScheduler(
		config.Scheduler, stats, pctx, cads, netevents, trackers, tls)
	if err != nil {
		log.Fatalf("Error creating scheduler: %s", err)
	}

	buildIndexes, err := config.BuildIndex.Build()
	if err != nil {
		log.Fatalf("Error building build-index upstream: %s", err)
	}

	tagClient := tagclient.NewClusterClient(buildIndexes, tls)

	transferer := transfer.NewReadOnlyTransferer(stats, cads, tagClient, sched)

	registry, err := config.Registry.Build(config.Registry.ReadOnlyParameters(transferer, cads, stats))
	if err != nil {
		log.Fatalf("Failed to init registry: %s", err)
	}

	agentServer := agentserver.New(config.AgentServer, stats, cads, sched, tagClient)
	addr := fmt.Sprintf(":%d", flags.AgentServerPort)
	log.Infof("Starting agent server on %s", addr)
	go func() {
		log.Fatal(http.ListenAndServe(addr, agentServer.Handler()))
	}()

	log.Info("Starting registry...")
	go func() {
		log.Fatal(registry.ListenAndServe())
	}()

	go heartbeat(stats)

	// Wipe log files created by the old nginx process which ran as root.
	// TODO(codyg): Swap these with the v2 log files once they are deleted.
	for _, name := range []string{
		"/var/log/kraken/kraken-agent/nginx-access.log",
		"/var/log/kraken/kraken-agent/nginx-error.log",
	} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			log.Warnf("Could not remove old root-owned nginx log: %s", err)
		}
	}

	log.Fatal(nginx.Run(config.Nginx, map[string]interface{}{
		"allowed_cidrs": config.AllowedCidrs,
		"port": flags.AgentRegistryPort,
		"registry_server": nginx.GetServer(
			config.Registry.Docker.HTTP.Net, config.Registry.Docker.HTTP.Addr),
		"registry_backup": config.RegistryBackup},
		nginx.WithTLS(config.TLS)))
}

// heartbeat periodically emits a counter metric which allows us to monitor the
// number of active agents.
func heartbeat(stats tally.Scope) {
	for {
		stats.Counter("heartbeat").Inc(1)
		time.Sleep(10 * time.Second)
	}
}
