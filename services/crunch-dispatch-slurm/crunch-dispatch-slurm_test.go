// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/arvadostest"
	"git.curoverse.com/arvados.git/sdk/go/dispatch"
	. "gopkg.in/check.v1"
)

// Gocheck boilerplate
func Test(t *testing.T) {
	TestingT(t)
}

var _ = Suite(&TestSuite{})
var _ = Suite(&MockArvadosServerSuite{})

type TestSuite struct{}
type MockArvadosServerSuite struct{}

var initialArgs []string

func (s *TestSuite) SetUpSuite(c *C) {
	initialArgs = os.Args
}

func (s *TestSuite) TearDownSuite(c *C) {
}

func (s *TestSuite) SetUpTest(c *C) {
	args := []string{"crunch-dispatch-slurm"}
	os.Args = args

	arvadostest.StartAPI()
	os.Setenv("ARVADOS_API_TOKEN", arvadostest.Dispatch1Token)
}

func (s *TestSuite) TearDownTest(c *C) {
	os.Args = initialArgs
	arvadostest.ResetEnv()
	arvadostest.StopAPI()
}

func (s *MockArvadosServerSuite) TearDownTest(c *C) {
	arvadostest.ResetEnv()
}

func (s *TestSuite) TestIntegrationNormal(c *C) {
	done := false
	container := s.integrationTest(c,
		func() *exec.Cmd {
			if done {
				return exec.Command("true")
			} else {
				return exec.Command("echo", "zzzzz-dz642-queuedcontainer 999000")
			}
		},
		nil,
		nil,
		[]string(nil),
		func(dispatcher *dispatch.Dispatcher, container arvados.Container) {
			dispatcher.UpdateState(container.UUID, dispatch.Running)
			time.Sleep(3 * time.Second)
			dispatcher.UpdateState(container.UUID, dispatch.Complete)
			done = true
		})
	c.Check(container.State, Equals, arvados.ContainerStateComplete)
}

func (s *TestSuite) TestIntegrationCancel(c *C) {
	var cmd *exec.Cmd
	var scancelCmdLine []string
	attempt := 0

	container := s.integrationTest(c,
		func() *exec.Cmd {
			if cmd != nil && cmd.ProcessState != nil {
				return exec.Command("true")
			} else {
				return exec.Command("echo", "zzzzz-dz642-queuedcontainer 999000")
			}
		},
		func(container arvados.Container) *exec.Cmd {
			if attempt++; attempt == 1 {
				return exec.Command("false")
			} else {
				scancelCmdLine = scancelFunc(container).Args
				cmd = exec.Command("echo")
				return cmd
			}
		},
		nil,
		[]string(nil),
		func(dispatcher *dispatch.Dispatcher, container arvados.Container) {
			dispatcher.UpdateState(container.UUID, dispatch.Running)
			time.Sleep(1 * time.Second)
			dispatcher.Arv.Update("containers", container.UUID,
				arvadosclient.Dict{
					"container": arvadosclient.Dict{"priority": 0}},
				nil)
		})
	c.Check(container.State, Equals, arvados.ContainerStateCancelled)
	c.Check(scancelCmdLine, DeepEquals, []string{"scancel", "--name=zzzzz-dz642-queuedcontainer"})
}

func (s *TestSuite) TestIntegrationMissingFromSqueue(c *C) {
	container := s.integrationTest(c,
		func() *exec.Cmd { return exec.Command("echo") },
		nil,
		nil,
		[]string{"sbatch",
			fmt.Sprintf("--job-name=%s", "zzzzz-dz642-queuedcontainer"),
			fmt.Sprintf("--mem=%d", 11445),
			fmt.Sprintf("--cpus-per-task=%d", 4),
			fmt.Sprintf("--tmp=%d", 45777),
			fmt.Sprintf("--nice=%d", 999000)},
		func(dispatcher *dispatch.Dispatcher, container arvados.Container) {
			dispatcher.UpdateState(container.UUID, dispatch.Running)
			time.Sleep(3 * time.Second)
			dispatcher.UpdateState(container.UUID, dispatch.Complete)
		})
	c.Check(container.State, Equals, arvados.ContainerStateCancelled)
}

func (s *TestSuite) TestSbatchFail(c *C) {
	container := s.integrationTest(c,
		func() *exec.Cmd { return exec.Command("echo") },
		nil,
		func(container arvados.Container) *exec.Cmd {
			return exec.Command("false")
		},
		[]string(nil),
		func(dispatcher *dispatch.Dispatcher, container arvados.Container) {
			dispatcher.UpdateState(container.UUID, dispatch.Running)
			dispatcher.UpdateState(container.UUID, dispatch.Complete)
		})
	c.Check(container.State, Equals, arvados.ContainerStateComplete)

	arv, err := arvadosclient.MakeArvadosClient()
	c.Assert(err, IsNil)

	var ll arvados.LogList
	err = arv.List("logs", arvadosclient.Dict{"filters": [][]string{
		{"object_uuid", "=", container.UUID},
		{"event_type", "=", "dispatch"},
	}}, &ll)
	c.Assert(len(ll.Items), Equals, 1)
}

func (s *TestSuite) integrationTest(c *C,
	newSqueueCmd func() *exec.Cmd,
	newScancelCmd func(arvados.Container) *exec.Cmd,
	newSbatchCmd func(arvados.Container) *exec.Cmd,
	sbatchCmdComps []string,
	runContainer func(*dispatch.Dispatcher, arvados.Container)) arvados.Container {
	arvadostest.ResetEnv()

	arv, err := arvadosclient.MakeArvadosClient()
	c.Assert(err, IsNil)

	var sbatchCmdLine []string

	// Override sbatchCmd
	defer func(orig func(arvados.Container) *exec.Cmd) {
		sbatchCmd = orig
	}(sbatchCmd)

	if newSbatchCmd != nil {
		sbatchCmd = newSbatchCmd
	} else {
		sbatchCmd = func(container arvados.Container) *exec.Cmd {
			sbatchCmdLine = sbatchFunc(container).Args
			return exec.Command("sh")
		}
	}

	// Override squeueCmd
	defer func(orig func() *exec.Cmd) {
		squeueCmd = orig
	}(squeueCmd)
	squeueCmd = newSqueueCmd

	// Override scancel
	defer func(orig func(arvados.Container) *exec.Cmd) {
		scancelCmd = orig
	}(scancelCmd)
	scancelCmd = newScancelCmd

	// Override scontrol
	defer func(orig func(arvados.Container) *exec.Cmd) {
		scontrolCmd = orig
	}(scontrolCmd)
	scontrolCmd = func(container arvados.Container) *exec.Cmd {
		return exec.Command("true")
	}

	// There should be one queued container
	params := arvadosclient.Dict{
		"filters": [][]string{{"state", "=", "Queued"}},
	}
	var containers arvados.ContainerList
	err = arv.List("containers", params, &containers)
	c.Check(err, IsNil)
	c.Check(len(containers.Items), Equals, 1)

	theConfig.CrunchRunCommand = []string{"echo"}

	ctx, cancel := context.WithCancel(context.Background())
	doneRun := make(chan struct{})

	dispatcher := dispatch.Dispatcher{
		Arv:        arv,
		PollPeriod: time.Duration(1) * time.Second,
		RunContainer: func(disp *dispatch.Dispatcher, ctr arvados.Container, status <-chan arvados.Container) {
			go func() {
				runContainer(disp, ctr)
				doneRun <- struct{}{}
			}()
			run(disp, ctr, status)
			cancel()
		},
	}

	sqCheck = &SqueueChecker{Period: 500 * time.Millisecond}

	err = dispatcher.Run(ctx)
	<-doneRun
	c.Assert(err, Equals, context.Canceled)

	sqCheck.Stop()

	c.Check(sbatchCmdLine, DeepEquals, sbatchCmdComps)

	// There should be no queued containers now
	err = arv.List("containers", params, &containers)
	c.Check(err, IsNil)
	c.Check(len(containers.Items), Equals, 0)

	// Previously "Queued" container should now be in "Complete" state
	var container arvados.Container
	err = arv.Get("containers", "zzzzz-dz642-queuedcontainer", nil, &container)
	c.Check(err, IsNil)
	return container
}

func (s *MockArvadosServerSuite) TestAPIErrorGettingContainers(c *C) {
	apiStubResponses := make(map[string]arvadostest.StubResponse)
	apiStubResponses["/arvados/v1/api_client_authorizations/current"] = arvadostest.StubResponse{200, `{"uuid":"` + arvadostest.Dispatch1AuthUUID + `"}`}
	apiStubResponses["/arvados/v1/containers"] = arvadostest.StubResponse{500, string(`{}`)}

	testWithServerStub(c, apiStubResponses, "echo", "Error getting list of containers")
}

func testWithServerStub(c *C, apiStubResponses map[string]arvadostest.StubResponse, crunchCmd string, expected string) {
	apiStub := arvadostest.ServerStub{apiStubResponses}

	api := httptest.NewServer(&apiStub)
	defer api.Close()

	arv := &arvadosclient.ArvadosClient{
		Scheme:    "http",
		ApiServer: api.URL[7:],
		ApiToken:  "abc123",
		Client:    &http.Client{Transport: &http.Transport{}},
		Retries:   0,
	}

	buf := bytes.NewBuffer(nil)
	log.SetOutput(io.MultiWriter(buf, os.Stderr))
	defer log.SetOutput(os.Stderr)

	theConfig.CrunchRunCommand = []string{crunchCmd}

	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := dispatch.Dispatcher{
		Arv:        arv,
		PollPeriod: time.Duration(1) * time.Second,
		RunContainer: func(disp *dispatch.Dispatcher, ctr arvados.Container, status <-chan arvados.Container) {
			go func() {
				time.Sleep(1 * time.Second)
				disp.UpdateState(ctr.UUID, dispatch.Running)
				disp.UpdateState(ctr.UUID, dispatch.Complete)
			}()
			run(disp, ctr, status)
			cancel()
		},
	}

	go func() {
		for i := 0; i < 80 && !strings.Contains(buf.String(), expected); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		cancel()
	}()

	err := dispatcher.Run(ctx)
	c.Assert(err, Equals, context.Canceled)

	c.Check(buf.String(), Matches, `(?ms).*`+expected+`.*`)
}

func (s *MockArvadosServerSuite) TestNoSuchConfigFile(c *C) {
	var config Config
	err := readConfig(&config, "/nosuchdir89j7879/8hjwr7ojgyy7")
	c.Assert(err, NotNil)
}

func (s *MockArvadosServerSuite) TestBadSbatchArgsConfig(c *C) {
	var config Config

	tmpfile, err := ioutil.TempFile(os.TempDir(), "config")
	c.Check(err, IsNil)
	defer os.Remove(tmpfile.Name())

	_, err = tmpfile.Write([]byte(`{"SbatchArguments": "oops this is not a string array"}`))
	c.Check(err, IsNil)

	err = readConfig(&config, tmpfile.Name())
	c.Assert(err, NotNil)
}

func (s *MockArvadosServerSuite) TestNoSuchArgInConfigIgnored(c *C) {
	var config Config

	tmpfile, err := ioutil.TempFile(os.TempDir(), "config")
	c.Check(err, IsNil)
	defer os.Remove(tmpfile.Name())

	_, err = tmpfile.Write([]byte(`{"NoSuchArg": "Nobody loves me, not one tiny hunk."}`))
	c.Check(err, IsNil)

	err = readConfig(&config, tmpfile.Name())
	c.Assert(err, IsNil)
	c.Check(0, Equals, len(config.SbatchArguments))
}

func (s *MockArvadosServerSuite) TestReadConfig(c *C) {
	var config Config

	tmpfile, err := ioutil.TempFile(os.TempDir(), "config")
	c.Check(err, IsNil)
	defer os.Remove(tmpfile.Name())

	args := []string{"--arg1=v1", "--arg2", "--arg3=v3"}
	argsS := `{"SbatchArguments": ["--arg1=v1",  "--arg2", "--arg3=v3"]}`
	_, err = tmpfile.Write([]byte(argsS))
	c.Check(err, IsNil)

	err = readConfig(&config, tmpfile.Name())
	c.Assert(err, IsNil)
	c.Check(3, Equals, len(config.SbatchArguments))
	c.Check(args, DeepEquals, config.SbatchArguments)
}

func (s *MockArvadosServerSuite) TestSbatchFuncWithNoConfigArgs(c *C) {
	testSbatchFuncWithArgs(c, nil)
}

func (s *MockArvadosServerSuite) TestSbatchFuncWithEmptyConfigArgs(c *C) {
	testSbatchFuncWithArgs(c, []string{})
}

func (s *MockArvadosServerSuite) TestSbatchFuncWithConfigArgs(c *C) {
	testSbatchFuncWithArgs(c, []string{"--arg1=v1", "--arg2"})
}

func testSbatchFuncWithArgs(c *C, args []string) {
	theConfig.SbatchArguments = append(theConfig.SbatchArguments, args...)

	container := arvados.Container{
		UUID:               "123",
		RuntimeConstraints: arvados.RuntimeConstraints{RAM: 250000000, VCPUs: 2},
		Priority:           1}
	sbatchCmd := sbatchFunc(container)

	var expected []string
	expected = append(expected, "sbatch")
	expected = append(expected, theConfig.SbatchArguments...)
	expected = append(expected, "--job-name=123", "--mem=239", "--cpus-per-task=2", "--tmp=0", "--nice=999000")

	c.Check(sbatchCmd.Args, DeepEquals, expected)
}

func (s *MockArvadosServerSuite) TestSbatchPartition(c *C) {
	theConfig.SbatchArguments = nil
	container := arvados.Container{
		UUID:                 "123",
		RuntimeConstraints:   arvados.RuntimeConstraints{RAM: 250000000, VCPUs: 1},
		SchedulingParameters: arvados.SchedulingParameters{Partitions: []string{"blurb", "b2"}},
		Priority:             1}
	sbatchCmd := sbatchFunc(container)

	var expected []string
	expected = append(expected, "sbatch")
	expected = append(expected, "--job-name=123", "--mem=239", "--cpus-per-task=1", "--tmp=0", "--nice=999000", "--partition=blurb,b2")

	c.Check(sbatchCmd.Args, DeepEquals, expected)
}
