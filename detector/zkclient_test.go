package detector

import (
	"errors"
	"github.com/gogo/protobuf/proto"
	log "github.com/golang/glog"
	util "github.com/mesos/mesos-go/mesosutil"
	"github.com/samuel/go-zookeeper/zk"
	"github.com/stretchr/testify/assert"
	"os"
	"strings"
	"testing"
	"time"
)

var test_zk_hosts = []string{"localhost:2181"}
var test_zk_path = "/test"

func TestZkClientNew(t *testing.T) {
	path := "/mesos"
	chEvent := make(chan zk.Event)
	connector := makeMockConnector(path, chEvent)

	c, err := newZkClient(test_zk_hosts, path)
	assert.NoError(t, err)
	assert.NotNil(t, c)
	assert.False(t, c.connected)
	c.conn = connector

}

// This test requires zookeeper to be running.
// You must also set env variable ZK_HOSTS to point to zk hosts.
// The zk package does not offer a way to mock its connection function.
func TestZkClientConnectIntegration(t *testing.T) {
	if os.Getenv("ZK_HOSTS") == "" {
		t.Skip("Skipping zk-server connection test: missing env ZK_HOSTS.")
	}
	hosts := strings.Split(os.Getenv("ZK_HOSTS"), ",")
	c, err := newZkClient(hosts, "/mesos")
	assert.NoError(t, err)
	err = c.connect()
	assert.NoError(t, err)

	err = c.connect()
	assert.NoError(t, err)
	assert.True(t, c.connected)
}

func TestZkClientConnect(t *testing.T) {
	c, err := makeZkClient()
	assert.NoError(t, err)
	assert.False(t, c.connected)
	c.connect()
	assert.True(t, c.connected)
}

func TestZkClientWatchChildren(t *testing.T) {
	c, err := makeZkClient()
	assert.NoError(t, err)
	err = c.connect()
	assert.NoError(t, err)
	wCh := make(chan struct{}, 1)
	c.childrenWatcher = zkChildrenWatcherFunc(func(zkc *zkClient, path string) {
		log.V(4).Infoln("Path", path, "changed!")
		children, err := c.list(path)
		assert.NoError(t, err)
		assert.Equal(t, 3, len(children))
		// assert sorted children
		assert.Equal(t, "a", children[0])
		assert.Equal(t, "d", children[1])
		assert.Equal(t, "x", children[2])
		wCh <- struct{}{}
	})

	err = c.watchChildren(".")
	assert.NoError(t, err)

	select {
	case <-wCh:
	case <-time.After(time.Millisecond * 700):
		panic("Waited too long...")
	}
}

func TestZkClientWatchErrors(t *testing.T) {
	path := "/test"
	ch := make(chan zk.Event, 1)
	ch <- zk.Event{
		Type: zk.EventNodeChildrenChanged,
		Path: "/test",
		Err:  errors.New("Event Error"),
	}

	c, err := makeZkClient()
	c.connected = true
	assert.NoError(t, err)
	c.conn = makeMockConnector(path, (<-chan zk.Event)(ch))
	wCh := make(chan struct{}, 1)
	c.errorWatcher = zkErrorWatcherFunc(func(zkc *zkClient, err error) {
		assert.Error(t, err)
		wCh <- struct{}{}
	})

	c.watchChildren(".")

	select {
	case <-wCh:
	case <-time.After(time.Millisecond * 700):
		panic("Waited too long...")
	}

}

func makeZkClient() (*zkClient, error) {
	ch0 := make(chan zk.Event, 1)
	ch1 := make(chan zk.Event, 1)

	ch0 <- zk.Event{
		State: zk.StateConnected,
		Path:  test_zk_path,
	}

	ch1 <- zk.Event{
		Type: zk.EventNodeChildrenChanged,
		Path: test_zk_path,
	}

	c, err := newZkClient(test_zk_hosts, test_zk_path)
	if err != nil {
		return nil, err
	}

	c.connFactory = zkConnFactoryFunc(func() (zkConnector, <-chan zk.Event, error) {
		log.V(2).Infof("**** Using zk.Conn adapter ****")
		connector := makeMockConnector(test_zk_path, ch1)
		return connector, ch0, nil
	})

	return c, nil
}

func makeMockConnector(path string, chEvent <-chan zk.Event) *MockZkConnector {
	log.V(2).Infoln("Making zkConnector mock.")
	conn := NewMockZkConnector()
	conn.On("Close").Return(nil)
	conn.On("ChildrenW", path).Return([]string{path}, &zk.Stat{}, chEvent, nil)
	conn.On("Children").Return([]string{"x", "a", "d"}, &zk.Stat{}, nil)

	miPb := util.NewMasterInfo("master@localhost:5050", 123456789, 400)
	data, err := proto.Marshal(miPb)
	if err != nil {
		panic(err)
	}

	conn.On("Get", path).Return(data, &zk.Stat{}, nil)

	return conn
}