package scheduler

import (
	"bytes"
	"code.google.com/p/gogoprotobuf/proto"
	"fmt"
	log "github.com/golang/glog"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/messenger"
	"github.com/mesos/mesos-go/upid"
	"github.com/mesos/mesos-go/util"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"reflect"
	"testing"
	"time"
)

var (
	master      = "127.0.0.1:8080"
	masterUpid  = "master(2)@" + master
	masterId    = "some-master-id-uuid"
	frameworkID = "some-framework-id-uuid"
	framework   = util.NewFrameworkInfo(
		"test-user",
		"test-name",
		util.NewFrameworkID(frameworkID),
	)
)

func makeMockServer(handler func(rsp http.ResponseWriter, req *http.Request)) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(handler))
	log.Error("Created test http server  ", server.URL)
	return server
}

// MockMaster to send a event messages to processes.
func generateMasterEvent(t *testing.T, targetPid *upid.UPID, message proto.Message) {
	messageName := reflect.TypeOf(message).Elem().Name()
	data, err := proto.Marshal(message)
	assert.NoError(t, err)
	hostport := net.JoinHostPort(targetPid.Host, targetPid.Port)
	targetURL := fmt.Sprintf("http://%s/%s/mesos.internal.%s", hostport, targetPid.ID, messageName)
	log.Infoln("MockMaster Sending message to", targetURL)
	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(data))
	assert.NoError(t, err)
	req.Header.Add("Libprocess-From", targetPid.String())
	req.Header.Add("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

func TestSchedulerDriverNew(t *testing.T) {
	masterAddr := "localhost:5050"
	mUpid, err := upid.Parse("master@" + masterAddr)
	assert.NoError(t, err)
	driver, err := NewMesosSchedulerDriver(&Scheduler{}, &mesos.FrameworkInfo{}, masterAddr, nil)
	assert.NotNil(t, driver)
	assert.NoError(t, err)
	assert.True(t, driver.MasterUPID.Equal(mUpid))
	user, _ := user.Current()
	assert.Equal(t, user.Username, driver.FrameworkInfo.GetUser())
	host, _ := os.Hostname()
	assert.Equal(t, host, driver.FrameworkInfo.GetHostname())
}

func TestSchedulerDriverNew_WithFrameworkInfo_Override(t *testing.T) {
	framework.Hostname = proto.String("local-host")
	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, "localhost:5050", nil)
	assert.NoError(t, err)
	assert.Equal(t, driver.FrameworkInfo.GetUser(), "test-user")
	assert.Equal(t, "local-host", driver.FrameworkInfo.GetHostname())
}

// ------------------- Http Message Handler Tests --------------------

func TestSchedulerDriverFrameworkRegisteredEvent(t *testing.T) {
	// start mock master server to handle connection
	server := makeMockServer(func(rsp http.ResponseWriter, req *http.Request) {
		log.Infoln("MockMaster - rcvd ", req.RequestURI)
		rsp.WriteHeader(http.StatusAccepted)
	})

	defer server.Close()
	url, _ := url.Parse(server.URL)

	ch := make(chan bool)
	sched := &Scheduler{
		Registered: func(dr SchedulerDriver, fw *mesos.FrameworkID, mi *mesos.MasterInfo) {
			assert.Equal(t, fw.GetValue(), framework.Id.GetValue())
			assert.Equal(t, mi.GetIp(), 123456)
			ch <- true
		},
	}

	driver, err := NewMesosSchedulerDriver(sched, framework, url.Host, nil)
	assert.NoError(t, err)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.Start())

	// Send a event to this SchedulerDriver (via http) to test handlers.
	pbMsg := &mesos.FrameworkRegisteredMessage{
		FrameworkId: framework.Id,
		MasterInfo:  util.NewMasterInfo("master", 123456, 1234),
	}
	generateMasterEvent(t, driver.self, pbMsg) // after this driver.connced=true
	<-time.After(time.Millisecond * 1)
	assert.True(t, driver.connected)
	select {
	case <-ch:
	case <-time.After(time.Millisecond * 2):
	}
}

// -------------------------------------------------------------------

func TestSchedulerDriverStartOK(t *testing.T) {
	sched := &Scheduler{}

	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(sched, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Start()
	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, stat)
}

func TestSchedulerDriverStartWithMessengerFailure(t *testing.T) {
	sched := &Scheduler{}

	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(fmt.Errorf("Failed to start messenger"))
	messenger.On("Stop").Return()

	driver, err := NewMesosSchedulerDriver(sched, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Start()
	assert.True(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, driver.status)
	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, stat)

}

func TestSchedulerDriverStartWithRegistrationFailure(t *testing.T) {
	sched := &Scheduler{}

	// Set expections and return values.
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(fmt.Errorf("messenger failed to send"))
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(sched, framework, master, nil)

	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Start()
	assert.True(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, driver.status)
	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, stat)

}

func TestSchedulerDriverStartIntegration(t *testing.T) {
	server := makeMockServer(func(rsp http.ResponseWriter, req *http.Request) {
		log.Infoln("RCVD request ", req.URL)

		data, err := ioutil.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("Missing RegisteredFramework data from scheduler.")
		}
		defer req.Body.Close()

		message := new(mesos.RegisterFrameworkMessage)
		err = proto.Unmarshal(data, message)
		if err != nil {
			t.Fatal("Problem unmarshaling expected RegisterFrameworkMessage")
		}

		assert.NotNil(t, message)
		info := message.GetFramework()
		assert.NotNil(t, info)
		assert.Equal(t, framework.GetName(), info.GetName())
		assert.Equal(t, framework.GetId().GetValue(), info.GetId().GetValue())
		rsp.WriteHeader(http.StatusOK)
	})
	defer server.Close()
	url, _ := url.Parse(server.URL)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, url.Host, nil)
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Start()

	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, stat)

	<-time.After(time.Millisecond * 7)
}

func TestSchedulerDriverJoinUnstarted(t *testing.T) {
	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Join()
	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, stat)
}

func TestSchedulerDriverJoinOK(t *testing.T) {
	// Set expections and return values.
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Start()
	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, stat)

	testCh := make(chan mesos.Status)
	go func() {
		stat := driver.Join()
		testCh <- stat
	}()

	close(driver.stopCh) // manually stopping
	stat = <-testCh      // when Stop() is called, stat will be DRIVER_STOPPED.
}

func TestSchedulerDriverRun(t *testing.T) {
	// Set expections and return values.
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	go func() {
		stat := driver.Run()
		assert.Equal(t, mesos.Status_DRIVER_STOPPED, stat)
	}()
	time.Sleep(time.Millisecond * 1)

	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.status)

	// close it all.
	driver.status = mesos.Status_DRIVER_STOPPED
	close(driver.stopCh)
	time.Sleep(time.Millisecond * 1)
}

func TestSchedulerDriverStopUnstarted(t *testing.T) {
	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.Stop(true)
	assert.True(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, stat)
}

func TestSchdulerDriverStopOK(t *testing.T) {
	// Set expections and return values.
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	go func() {
		stat := driver.Run()
		assert.Equal(t, mesos.Status_DRIVER_STOPPED, stat)
	}()
	time.Sleep(time.Millisecond * 1)

	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.status)

	driver.Stop(false)
	time.Sleep(time.Millisecond * 1)

	assert.True(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_STOPPED, driver.status)
}

func TestSchdulerDriverAbort(t *testing.T) {
	// Set expections and return values.
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	go func() {
		stat := driver.Run()
		assert.Equal(t, mesos.Status_DRIVER_ABORTED, stat)
	}()
	time.Sleep(time.Millisecond * 1)
	driver.connected = true // simulated

	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.status)

	stat := driver.Abort()
	time.Sleep(time.Millisecond * 1)

	assert.True(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_ABORTED, stat)
	assert.Equal(t, mesos.Status_DRIVER_ABORTED, driver.status)
}

func TestLunchTasksUnstarted(t *testing.T) {
	// Set expections and return values.
	messenger := messenger.NewMockedMessenger()

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	stat := driver.LaunchTasks(
		&mesos.OfferID{},
		[]*mesos.TaskInfo{},
		&mesos.Filters{},
	)

	assert.Equal(t, mesos.Status_DRIVER_NOT_STARTED, stat)
}

func TestLaunchTasksWithError(t *testing.T) {
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("Send").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	go func() {
		driver.Run()
	}()
	time.Sleep(time.Millisecond * 1)
	driver.connected = true // simulated
	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.status)

	// trigger error
	messenger.On("Send").Return(fmt.Errorf("Unable to send message"))

	task := util.NewTaskInfo(
		"simple-task",
		util.NewTaskID("simpe-task-1"),
		util.NewSlaveID("slave-1"),
		[]*mesos.Resource{util.NewScalarResource("mem", 400)},
	)
	task.Command = util.NewCommandInfo("pwd")
	tasks := []*mesos.TaskInfo{task}

	stat := driver.LaunchTasks(
		&mesos.OfferID{},
		tasks,
		&mesos.Filters{},
	)

	assert.Equal(t, mesos.Status_DRIVER_RUNNING, stat)

}

func TestLaunchTasks(t *testing.T) {
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	go func() {
		driver.Run()
	}()
	time.Sleep(time.Millisecond * 1)
	driver.connected = true // simulated
	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.status)

	task := util.NewTaskInfo(
		"simple-task",
		util.NewTaskID("simpe-task-1"),
		util.NewSlaveID("slave-1"),
		[]*mesos.Resource{util.NewScalarResource("mem", 400)},
	)
	task.Command = util.NewCommandInfo("pwd")
	tasks := []*mesos.TaskInfo{task}

	stat := driver.LaunchTasks(
		&mesos.OfferID{},
		tasks,
		&mesos.Filters{},
	)

	assert.Equal(t, mesos.Status_DRIVER_RUNNING, stat)

}

func TestKillTask(t *testing.T) {
	messenger := messenger.NewMockedMessenger()
	messenger.On("Start").Return(nil)
	messenger.On("UPID").Return(&upid.UPID{})
	messenger.On("Send").Return(nil)
	messenger.On("Stop").Return(nil)

	driver, err := NewMesosSchedulerDriver(&Scheduler{}, framework, master, nil)
	driver.messenger = messenger
	assert.NoError(t, err)
	assert.True(t, driver.stopped)

	go func() {
		driver.Run()
	}()
	time.Sleep(time.Millisecond * 1)
	driver.connected = true // simulated
	assert.False(t, driver.stopped)
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, driver.status)

	stat := driver.KillTask(util.NewTaskID("test-task-1"))
	assert.Equal(t, mesos.Status_DRIVER_RUNNING, stat)
}