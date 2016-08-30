package cluster

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"

	"kandalf/config"
	"kandalf/logger"
	"kandalf/runnable"
	"kandalf/workers"
)

type Cluster struct {
	*runnable.RunnableWorker

	raft  *raft.Raft
	mutex *sync.Mutex
}

// Returns new clustered worker
func NewCluster(clusterNodes []string) *Cluster {
	var err error

	// Cluster settings
	clusterBindHost := config.Instance().UString("cluster.bind_host", "")
	clusterBindPort := config.Instance().UInt("cluster.bind_port", 11291)
	clusterDataDir := config.Instance().UString("cluster.data_dir", "/var/lib/kandalf")
	clusterMaxPool := config.Instance().UInt("cluster.max_pool", 3)
	clusterNbSnapshot := config.Instance().UInt("cluster.nb_snapshot", 2)
	clusterTimeout, err := time.ParseDuration(config.Instance().UString("cluster.timeout", "10s"))
	if err != nil {
		clusterTimeout = 10 * time.Second
	}

	if len(clusterBindHost) == 0 {
		clusterBindHost, err = getFirstLocalAddr()
		if err != nil {
			logger.Instance().
				WithError(err).
				Fatal("An error occured while getting local address")
		}
	}

	clusterBind := fmt.Sprintf("%s:%d", clusterBindHost, clusterBindPort)
	addr, err := net.ResolveTCPAddr("tcp", clusterBind)
	if err != nil {
		logger.Instance().
			WithError(err).
			WithField("addr", clusterBind).
			Fatal("An error occured while resolving address")
	}

	snapshots, err := raft.NewFileSnapshotStore(
		clusterDataDir,
		clusterNbSnapshot,
		logger.Instance().Writer())

	if err != nil {
		logger.Instance().
			WithError(err).
			WithFields(logrus.Fields{
				"data_dir":    clusterDataDir,
				"nb_snapshot": clusterNbSnapshot,
			}).
			Fatal("An error occurred while creating snapshots storage")
	}

	// Create the log store and stable store.
	boltPath := filepath.Join(clusterDataDir, "raft.db")
	boltStore, err := raftboltdb.NewBoltStore(boltPath)
	if err != nil {
		logger.Instance().
			WithError(err).
			WithField("path", boltPath).
			Fatal("An error occurred while creating BoltDB store")
	}

	transport, err := raft.NewTCPTransport(
		clusterBind,
		addr,
		clusterMaxPool,
		clusterTimeout,
		logger.Instance().Writer())

	if err != nil {
		logger.Instance().
			WithError(err).
			WithFields(logrus.Fields{
				"addr":     clusterBind,
				"max_pool": clusterMaxPool,
				"timeout":  clusterTimeout.String(),
			}).
			Fatal("An error occurred while creating raft TCP transport")
	}

	// Peer storage
	peerStore := raft.NewJSONPeers(clusterDataDir, transport)
	peerStore.SetPeers(clusterNodes)

	// Instantiate the Raft systems.
	cnf := raft.DefaultConfig()
	if len(clusterNodes) <= 1 {
		cnf.EnableSingleNode = true
		cnf.DisableBootstrapAfterElect = false
	}

	ra, err := raft.NewRaft(cnf, newFsm(), boltStore, boltStore, snapshots, peerStore, transport)
	if err != nil {
		logger.Instance().
			WithError(err).
			Fatal("An error occurred while instantiating raft")
	}

	c := &Cluster{
		raft:  ra,
		mutex: &sync.Mutex{},
	}

	c.RunnableWorker = runnable.NewRunnableWorker(c.doRun)

	return c
}

func (cl *Cluster) doRun(wgMain *sync.WaitGroup, dieMain chan bool) {
	go cl.listenForLeadership(wgMain, dieMain)

	for cl.RunnableWorker.IsWorking {
		time.Sleep(config.InfiniteCycleTimeout)
	}
}

// Here we're just listening for cluster state changes
// If node becomes leader, we'll launch RabbitMQ consumer
func (cl *Cluster) listenForLeadership(wgMain *sync.WaitGroup, dieMain chan bool) {
	var (
		becameLeader bool
		worker       *workers.Worker
	)

	becameLeader = <-cl.raft.LeaderCh()

	if becameLeader {
		// Stop the old worker if it exists
		if worker != nil {
			worker.RunnableWorker.IsWorking = false
			worker.IsWorking = false
		}

		worker = workers.NewWorker()
		worker.Run(wgMain, dieMain)
	}
}

// Returns first found IP address
func getFirstLocalAddr() (result string, err error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()

		if err != nil {
			continue
		}

		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				result = v.IP.String()
			case *net.IPAddr:
				result = v.IP.String()
			}

			if len(result) > 0 {
				break
			}
		}
	}

	if len(result) == 0 {
		return "", errors.New("Unable to find local address to bind")
	}

	return result, nil
}