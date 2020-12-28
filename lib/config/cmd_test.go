// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package config

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"

	"git.arvados.org/arvados.git/lib/cmd"
	check "gopkg.in/check.v1"
)

var _ = check.Suite(&CommandSuite{})

var (
	// Commands must satisfy cmd.Handler interface
	_ cmd.Handler = dumpCommand{}
	_ cmd.Handler = checkCommand{}
)

type CommandSuite struct{}

func (s *CommandSuite) SetUpSuite(c *check.C) {
	os.Unsetenv("ARVADOS_API_HOST")
	os.Unsetenv("ARVADOS_API_HOST_INSECURE")
	os.Unsetenv("ARVADOS_API_TOKEN")
}

func (s *CommandSuite) TestDump_BadArg(c *check.C) {
	var stderr bytes.Buffer
	code := DumpCommand.RunCommand("arvados config-dump", []string{"-badarg"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	c.Check(code, check.Equals, 2)
	c.Check(stderr.String(), check.Matches, `(?ms)flag provided but not defined: -badarg\nUsage:\n.*`)
}

func (s *CommandSuite) TestDump_EmptyInput(c *check.C) {
	var stdout, stderr bytes.Buffer
	code := DumpCommand.RunCommand("arvados config-dump", []string{"-config", "-"}, &bytes.Buffer{}, &stdout, &stderr)
	c.Check(code, check.Equals, 1)
	c.Check(stderr.String(), check.Matches, `config does not define any clusters\n`)
}

func (s *CommandSuite) TestCheck_NoWarnings(c *check.C) {
	var stdout, stderr bytes.Buffer
	in := `
Clusters:
 z1234:
  ManagementToken: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  SystemRootToken: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  API:
    MaxItemsPerResponse: 1234
  Collections:
    BlobSigningKey: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  PostgreSQL:
    Connection:
      sslmode: require
  Services:
    RailsAPI:
      InternalURLs:
        "http://0.0.0.0:8000": {}
  Workbench:
    UserProfileFormFields:
      color:
        Type: select
        Options:
          fuchsia: {}
    ApplicationMimetypesWithViewIcon:
      whitespace: {}
`
	code := CheckCommand.RunCommand("arvados config-check", []string{"-config", "-"}, bytes.NewBufferString(in), &stdout, &stderr)
	c.Check(code, check.Equals, 0)
	c.Check(stdout.String(), check.Equals, "")
	c.Check(stderr.String(), check.Equals, "")
}

func (s *CommandSuite) TestCheck_DeprecatedKeys(c *check.C) {
	var stdout, stderr bytes.Buffer
	in := `
Clusters:
 z1234:
  RequestLimits:
    MaxItemsPerResponse: 1234
`
	code := CheckCommand.RunCommand("arvados config-check", []string{"-config", "-"}, bytes.NewBufferString(in), &stdout, &stderr)
	c.Check(code, check.Equals, 1)
	c.Check(stdout.String(), check.Matches, `(?ms).*\n\- +.*MaxItemsPerResponse: 1000\n\+ +MaxItemsPerResponse: 1234\n.*`)
}

func (s *CommandSuite) TestCheck_OldKeepstoreConfigFile(c *check.C) {
	f, err := ioutil.TempFile("", "")
	c.Assert(err, check.IsNil)
	defer os.Remove(f.Name())

	io.WriteString(f, "Listen: :12345\nDebug: true\n")

	var stdout, stderr bytes.Buffer
	in := `
Clusters:
 z1234:
  SystemLogs:
    LogLevel: info
`
	code := CheckCommand.RunCommand("arvados config-check", []string{"-config", "-", "-legacy-keepstore-config", f.Name()}, bytes.NewBufferString(in), &stdout, &stderr)
	c.Check(code, check.Equals, 1)
	c.Check(stdout.String(), check.Matches, `(?ms).*\n\- +.*LogLevel: info\n\+ +LogLevel: debug\n.*`)
	c.Check(stderr.String(), check.Matches, `(?ms).*you should remove the legacy keepstore config file.*\n`)
}

func (s *CommandSuite) TestCheck_UnknownKey(c *check.C) {
	var stdout, stderr bytes.Buffer
	in := `
Clusters:
 z1234:
  Bogus1: foo
  BogusSection:
    Bogus2: foo
  API:
    Bogus3:
     Bogus4: true
  PostgreSQL:
    ConnectionPool:
      {Bogus5: true}
`
	code := CheckCommand.RunCommand("arvados config-check", []string{"-config", "-"}, bytes.NewBufferString(in), &stdout, &stderr)
	c.Log(stderr.String())
	c.Check(code, check.Equals, 1)
	c.Check(stderr.String(), check.Matches, `(?ms).*deprecated or unknown config entry: Clusters.z1234.Bogus1"\n.*`)
	c.Check(stderr.String(), check.Matches, `(?ms).*deprecated or unknown config entry: Clusters.z1234.BogusSection"\n.*`)
	c.Check(stderr.String(), check.Matches, `(?ms).*deprecated or unknown config entry: Clusters.z1234.API.Bogus3"\n.*`)
	c.Check(stderr.String(), check.Matches, `(?ms).*unexpected object in config entry: Clusters.z1234.PostgreSQL.ConnectionPool"\n.*`)
}

func (s *CommandSuite) TestDump_Formatting(c *check.C) {
	var stdout, stderr bytes.Buffer
	in := `
Clusters:
 z1234:
  Containers:
   CloudVMs:
    TimeoutBooting: 600s
  Services:
   Controller:
    InternalURLs:
     http://localhost:12345: {}
`
	code := DumpCommand.RunCommand("arvados config-dump", []string{"-config", "-"}, bytes.NewBufferString(in), &stdout, &stderr)
	c.Check(code, check.Equals, 0)
	c.Check(stdout.String(), check.Matches, `(?ms).*TimeoutBooting: 10m\n.*`)
	c.Check(stdout.String(), check.Matches, `(?ms).*http://localhost:12345/: {}\n.*`)
}

func (s *CommandSuite) TestDump_UnknownKey(c *check.C) {
	var stdout, stderr bytes.Buffer
	in := `
Clusters:
 z1234:
  UnknownKey: foobar
  ManagementToken: secret
`
	code := DumpCommand.RunCommand("arvados config-dump", []string{"-config", "-"}, bytes.NewBufferString(in), &stdout, &stderr)
	c.Check(code, check.Equals, 0)
	c.Check(stderr.String(), check.Matches, `(?ms).*deprecated or unknown config entry: Clusters.z1234.UnknownKey.*`)
	c.Check(stdout.String(), check.Matches, `(?ms)(.*\n)?Clusters:\n  z1234:\n.*`)
	c.Check(stdout.String(), check.Matches, `(?ms).*\n *ManagementToken: secret\n.*`)
	c.Check(stdout.String(), check.Not(check.Matches), `(?ms).*UnknownKey.*`)
}
