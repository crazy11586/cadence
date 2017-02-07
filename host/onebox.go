package host

import (
	"sync"
	"time"

	"code.uber.internal/devexp/minions/common/persistence"
	"code.uber.internal/devexp/minions/common/service"
	"code.uber.internal/devexp/minions/service/frontend"
	"code.uber.internal/devexp/minions/service/history"
	"code.uber.internal/devexp/minions/service/matching"
	"github.com/uber-common/bark"
	"github.com/uber-go/tally"
	tchannel "github.com/uber/tchannel-go"
	"github.com/uber/tchannel-go/thrift"
)

// Cadence hosts all of cadence services in one process
type Cadence interface {
	Start() error
	Stop()
	FrontendAddress() string
	MatchingServiceAddress() string
	HistoryServiceAddress() string
}

type cadenceImpl struct {
	frontendHandler       *frontend.WorkflowHandler
	matchingHandler       *matching.Handler
	historyHandler        *history.Handler
	numberOfHistoryShards int
	logger                bark.Logger
	shardMgr              persistence.ShardManager
	taskMgr               persistence.TaskManager
	executionMgrFactory   persistence.ExecutionManagerFactory
	shutdownCh            chan struct{}
	shutdownWG            sync.WaitGroup
}

// NewCadence returns an instance that hosts full cadence in one process
func NewCadence(shardMgr persistence.ShardManager, executionMgrFactory persistence.ExecutionManagerFactory,
	taskMgr persistence.TaskManager, numberOfHistoryShards int, logger bark.Logger) Cadence {
	return &cadenceImpl{
		numberOfHistoryShards: numberOfHistoryShards,
		logger:                logger,
		shardMgr:              shardMgr,
		taskMgr:               taskMgr,
		executionMgrFactory:   executionMgrFactory,
		shutdownCh:            make(chan struct{}),
	}
}

func (c *cadenceImpl) Start() error {
	var rpHosts []string
	rpHosts = append(rpHosts, c.FrontendAddress())
	rpHosts = append(rpHosts, c.MatchingServiceAddress())
	rpHosts = append(rpHosts, c.HistoryServiceAddress())

	var startWG sync.WaitGroup
	startWG.Add(2)
	go c.startHistory(c.logger, c.shardMgr, c.executionMgrFactory, rpHosts, &startWG)
	go c.startMatching(c.logger, c.taskMgr, rpHosts, &startWG)
	startWG.Wait()

	startWG.Add(1)
	go c.startFrontend(c.logger, rpHosts, &startWG)
	startWG.Wait()
	// Allow some time for the ring to stabilize
	// TODO: remove this after adding automatic retries on transient errors in clients
	time.Sleep(time.Second * 5)
	return nil
}

func (c *cadenceImpl) Stop() {
	c.shutdownWG.Add(3)
	c.frontendHandler.Stop()
	c.historyHandler.Stop()
	c.matchingHandler.Stop()
	close(c.shutdownCh)
	c.shutdownWG.Wait()
}

func (c *cadenceImpl) FrontendAddress() string {
	return "127.0.0.1:7104"
}

func (c *cadenceImpl) HistoryServiceAddress() string {
	return "127.0.0.1:7105"
}

func (c *cadenceImpl) MatchingServiceAddress() string {
	return "127.0.0.1:7106"
}

func (c *cadenceImpl) startFrontend(logger bark.Logger, rpHosts []string, startWG *sync.WaitGroup) {
	tchanFactory := func(sName string, thriftServices []thrift.TChanServer) (*tchannel.Channel, *thrift.Server) {
		return c.createTChannel(sName, c.FrontendAddress(), thriftServices)
	}
	scope := tally.NewTestScope("cadence-frontend", make(map[string]string))
	service := service.New("cadence-frontend", logger, scope, tchanFactory, rpHosts, c.numberOfHistoryShards)
	var thriftServices []thrift.TChanServer
	c.frontendHandler, thriftServices = frontend.NewWorkflowHandler(service)
	err := c.frontendHandler.Start(thriftServices)
	if err != nil {
		c.logger.WithField("error", err).Fatal("Failed to start frontend")
	}
	startWG.Done()
	<-c.shutdownCh
	c.shutdownWG.Done()
}

func (c *cadenceImpl) startHistory(logger bark.Logger, shardMgr persistence.ShardManager,
	executionMgrFactory persistence.ExecutionManagerFactory, rpHosts []string, startWG *sync.WaitGroup) {
	tchanFactory := func(sName string, thriftServices []thrift.TChanServer) (*tchannel.Channel, *thrift.Server) {
		return c.createTChannel(sName, c.HistoryServiceAddress(), thriftServices)
	}
	scope := tally.NewTestScope("cadence-history", make(map[string]string))
	service := service.New("cadence-history", logger, scope, tchanFactory, rpHosts, c.numberOfHistoryShards)
	var thriftServices []thrift.TChanServer
	c.historyHandler, thriftServices = history.NewHandler(service, shardMgr, executionMgrFactory, c.numberOfHistoryShards, false)
	c.historyHandler.Start(thriftServices)
	startWG.Done()
	<-c.shutdownCh
	c.shutdownWG.Done()
}

func (c *cadenceImpl) startMatching(logger bark.Logger, taskMgr persistence.TaskManager,
	rpHosts []string, startWG *sync.WaitGroup) {
	tchanFactory := func(sName string, thriftServices []thrift.TChanServer) (*tchannel.Channel, *thrift.Server) {
		return c.createTChannel(sName, c.MatchingServiceAddress(), thriftServices)
	}
	scope := tally.NewTestScope("cadence-matching", make(map[string]string))
	service := service.New("cadence-matching", logger, scope, tchanFactory, rpHosts, c.numberOfHistoryShards)
	var thriftServices []thrift.TChanServer
	c.matchingHandler, thriftServices = matching.NewHandler(taskMgr, service)
	c.matchingHandler.Start(thriftServices)
	startWG.Done()
	<-c.shutdownCh
	c.shutdownWG.Done()
}

func (c *cadenceImpl) createTChannel(sName string, hostPort string,
	thriftServices []thrift.TChanServer) (*tchannel.Channel, *thrift.Server) {
	ch, err := tchannel.NewChannel(sName, nil)
	if err != nil {
		c.logger.WithField("error", err).Fatal("Failed to create TChannel")
	}
	server := thrift.NewServer(ch)
	for _, thriftService := range thriftServices {
		server.Register(thriftService)
	}

	err = ch.ListenAndServe(hostPort)
	if err != nil {
		c.logger.WithField("error", err).Fatal("Failed to listen on tchannel")
	}
	return ch, server
}
