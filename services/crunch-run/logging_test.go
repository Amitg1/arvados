package main

import (
	"fmt"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	. "gopkg.in/check.v1"
	"strings"
	"testing"
	"time"
)

type LoggingTestSuite struct{}

type TestTimestamper struct {
	count int
}

func (this *TestTimestamper) Timestamp(t time.Time) string {
	this.count += 1
	t, err := time.ParseInLocation(time.RFC3339Nano, fmt.Sprintf("2015-12-29T15:51:45.%09dZ", this.count), t.Location())
	if err != nil {
		panic(err)
	}
	return RFC3339Timestamp(t)
}

// Gocheck boilerplate
var _ = Suite(&LoggingTestSuite{})

func (s *LoggingTestSuite) TestWriteLogs(c *C) {
	api := &ArvTestClient{}
	kc := &KeepTestClient{}
	cr := NewContainerRunner(api, kc, nil, "zzzzz-zzzzzzzzzzzzzzz")
	cr.CrunchLog.Timestamper = (&TestTimestamper{}).Timestamp

	cr.CrunchLog.Print("Hello world!")
	cr.CrunchLog.Print("Goodbye")
	cr.CrunchLog.Close()

	c.Check(api.Calls, Equals, 1)

	mt, err := cr.LogCollection.ManifestText()
	c.Check(err, IsNil)
	c.Check(mt, Equals, ". 74561df9ae65ee9f35d5661d42454264+83 0:83:crunch-run.txt\n")

	logtext := "2015-12-29T15:51:45.000000001Z Hello world!\n" +
		"2015-12-29T15:51:45.000000002Z Goodbye\n"

	c.Check(api.Content[0]["log"].(arvadosclient.Dict)["event_type"], Equals, "crunch-run")
	c.Check(api.Content[0]["log"].(arvadosclient.Dict)["properties"].(map[string]string)["text"], Equals, logtext)
	c.Check(string(kc.Content), Equals, logtext)
}

func (s *LoggingTestSuite) TestWriteLogsLarge(c *C) {
	if testing.Short() {
		return
	}
	api := &ArvTestClient{}
	kc := &KeepTestClient{}
	cr := NewContainerRunner(api, kc, nil, "zzzzz-zzzzzzzzzzzzzzz")
	cr.CrunchLog.Timestamper = (&TestTimestamper{}).Timestamp
	cr.CrunchLog.Immediate = nil

	for i := 0; i < 2000000; i++ {
		cr.CrunchLog.Printf("Hello %d", i)
	}
	cr.CrunchLog.Print("Goodbye")
	cr.CrunchLog.Close()

	c.Check(api.Calls > 1, Equals, true)
	c.Check(api.Calls < 2000000, Equals, true)

	mt, err := cr.LogCollection.ManifestText()
	c.Check(err, IsNil)
	c.Check(mt, Equals, ". 9c2c05d1fae6aaa8af85113ba725716d+67108864 80b821383a07266c2a66a4566835e26e+21780065 0:88888929:crunch-run.txt\n")
}

func (s *LoggingTestSuite) TestWriteMultipleLogs(c *C) {
	api := &ArvTestClient{}
	kc := &KeepTestClient{}
	cr := NewContainerRunner(api, kc, nil, "zzzzz-zzzzzzzzzzzzzzz")
	ts := &TestTimestamper{}
	cr.CrunchLog.Timestamper = ts.Timestamp
	stdout := NewThrottledLogger(cr.NewLogWriter("stdout"))
	stdout.Timestamper = ts.Timestamp

	cr.CrunchLog.Print("Hello world!")
	stdout.Print("Doing stuff")
	cr.CrunchLog.Print("Goodbye")
	stdout.Print("Blurb")
	cr.CrunchLog.Close()
	stdout.Close()

	logText := make(map[string]string)
	for _, content := range api.Content {
		log := content["log"].(arvadosclient.Dict)
		logText[log["event_type"].(string)] += log["properties"].(map[string]string)["text"]
	}

	c.Check(logText["crunch-run"], Equals, `2015-12-29T15:51:45.000000001Z Hello world!
2015-12-29T15:51:45.000000003Z Goodbye
`)
	c.Check(logText["stdout"], Equals, `2015-12-29T15:51:45.000000002Z Doing stuff
2015-12-29T15:51:45.000000004Z Blurb
`)

	mt, err := cr.LogCollection.ManifestText()
	c.Check(err, IsNil)
	c.Check(mt, Equals, ""+
		". 408672f5b5325f7d20edfbf899faee42+83 0:83:crunch-run.txt\n"+
		". c556a293010069fa79a6790a931531d5+80 0:80:stdout.txt\n")
}

func (s *LoggingTestSuite) TestWriteLogsWithRateLimitThrottleBytes(c *C) {
	discoveryMap["crunchLogThrottleBytes"] = float64(50)
	defer func() {
		discoveryMap["crunchLogThrottleBytes"] = float64(65536)
	}()

	api := &ArvTestClient{}
	kc := &KeepTestClient{}
	cr := NewContainerRunner(api, kc, nil, "zzzzz-zzzzzzzzzzzzzzz")
	cr.CrunchLog.Timestamper = (&TestTimestamper{}).Timestamp

	cr.CrunchLog.Print("Hello world!")
	cr.CrunchLog.Print("Goodbye")
	cr.CrunchLog.Close()

	c.Check(api.Calls, Equals, 1)

	mt, err := cr.LogCollection.ManifestText()
	c.Check(err, IsNil)
	c.Check(mt, Equals, ". 74561df9ae65ee9f35d5661d42454264+83 0:83:crunch-run.txt\n")

	logtext := "2015-12-29T15:51:45.000000001Z Hello world!\n" +
		"2015-12-29T15:51:45.000000002Z Goodbye\n"

	c.Check(api.Content[0]["log"].(arvadosclient.Dict)["event_type"], Equals, "crunch-run")
	stderrLog := api.Content[0]["log"].(arvadosclient.Dict)["properties"].(map[string]string)["text"]
	c.Check(true, Equals, strings.Contains(stderrLog, "Exceeded rate 50 bytes per 60 seconds"))
	c.Check(string(kc.Content), Equals, logtext)
}

func (s *LoggingTestSuite) TestWriteLogsWithRateLimitThrottleLines(c *C) {
	discoveryMap["crunchLogThrottleLines"] = float64(1)
	defer func() {
		discoveryMap["crunchLogThrottleLines"] = float64(1024)
	}()

	api := &ArvTestClient{}
	kc := &KeepTestClient{}
	cr := NewContainerRunner(api, kc, nil, "zzzzz-zzzzzzzzzzzzzzz")
	cr.CrunchLog.Timestamper = (&TestTimestamper{}).Timestamp

	cr.CrunchLog.Print("Hello world!")
	cr.CrunchLog.Print("Goodbye")
	cr.CrunchLog.Close()

	c.Check(api.Calls, Equals, 1)

	mt, err := cr.LogCollection.ManifestText()
	c.Check(err, IsNil)
	c.Check(mt, Equals, ". 74561df9ae65ee9f35d5661d42454264+83 0:83:crunch-run.txt\n")

	logtext := "2015-12-29T15:51:45.000000001Z Hello world!\n" +
		"2015-12-29T15:51:45.000000002Z Goodbye\n"

	c.Check(api.Content[0]["log"].(arvadosclient.Dict)["event_type"], Equals, "crunch-run")
	stderrLog := api.Content[0]["log"].(arvadosclient.Dict)["properties"].(map[string]string)["text"]
	c.Check(true, Equals, strings.Contains(stderrLog, "Exceeded rate 1 lines per 60 seconds"))
	c.Check(string(kc.Content), Equals, logtext)
}
