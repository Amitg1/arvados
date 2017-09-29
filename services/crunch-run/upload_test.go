// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"io/ioutil"
	"log"
	"os"
	"sync"
	"syscall"

	. "gopkg.in/check.v1"
)

type UploadTestSuite struct{}

// Gocheck boilerplate
var _ = Suite(&UploadTestSuite{})

func (s *TestSuite) TestSimpleUpload(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	ioutil.WriteFile(tmpdir+"/"+"file1.txt", []byte("foo"), 0600)

	cw := CollectionWriter{0, &KeepTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))
	c.Check(err, IsNil)
	c.Check(str, Equals, ". acbd18db4cc2f85cedef654fccc4a4d8+3 0:3:file1.txt\n")
}

func (s *TestSuite) TestSimpleUploadThreefiles(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	for _, err := range []error{
		ioutil.WriteFile(tmpdir+"/"+"file1.txt", []byte("foo"), 0600),
		ioutil.WriteFile(tmpdir+"/"+"file2.txt", []byte("bar"), 0600),
		os.Symlink("./file2.txt", tmpdir+"/file3.txt"),
		syscall.Mkfifo(tmpdir+"/ignore.fifo", 0600),
	} {
		c.Assert(err, IsNil)
	}

	cw := CollectionWriter{0, &KeepTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))

	c.Check(err, IsNil)
	c.Check(str, Equals, ". aa65a413921163458c52fea478d5d3ee+9 0:3:file1.txt 3:3:file2.txt 6:3:file3.txt\n")
}

func (s *TestSuite) TestSimpleUploadSubdir(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	os.Mkdir(tmpdir+"/subdir", 0700)

	ioutil.WriteFile(tmpdir+"/"+"file1.txt", []byte("foo"), 0600)
	ioutil.WriteFile(tmpdir+"/subdir/file2.txt", []byte("bar"), 0600)

	cw := CollectionWriter{0, &KeepTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))

	c.Check(err, IsNil)

	// streams can get added in either order because of scheduling
	// of goroutines.
	if str != `. acbd18db4cc2f85cedef654fccc4a4d8+3 0:3:file1.txt
./subdir 37b51d194a7513e45b56f6524f2d51f2+3 0:3:file2.txt
` && str != `./subdir 37b51d194a7513e45b56f6524f2d51f2+3 0:3:file2.txt
. acbd18db4cc2f85cedef654fccc4a4d8+3 0:3:file1.txt
` {
		c.Error("Did not get expected manifest text")
	}
}

func (s *TestSuite) TestSimpleUploadLarge(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	file, _ := os.Create(tmpdir + "/" + "file1.txt")
	data := make([]byte, 1024*1024-1)
	for i := range data {
		data[i] = byte(i % 10)
	}
	for i := 0; i < 65; i++ {
		file.Write(data)
	}
	file.Close()

	ioutil.WriteFile(tmpdir+"/"+"file2.txt", []byte("bar"), 0600)

	cw := CollectionWriter{0, &KeepTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))

	c.Check(err, IsNil)
	c.Check(str, Equals, ". 00ecf01e0d93385115c9f8bed757425d+67108864 485cd630387b6b1846fe429f261ea05f+1048514 0:68157375:file1.txt 68157375:3:file2.txt\n")
}

func (s *TestSuite) TestUploadEmptySubdir(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	os.Mkdir(tmpdir+"/subdir", 0700)

	ioutil.WriteFile(tmpdir+"/"+"file1.txt", []byte("foo"), 0600)

	cw := CollectionWriter{0, &KeepTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))

	c.Check(err, IsNil)
	c.Check(str, Equals, `. acbd18db4cc2f85cedef654fccc4a4d8+3 0:3:file1.txt
`)
}

func (s *TestSuite) TestUploadEmptyFile(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	ioutil.WriteFile(tmpdir+"/"+"file1.txt", []byte(""), 0600)

	cw := CollectionWriter{0, &KeepTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))

	c.Check(err, IsNil)
	c.Check(str, Equals, `. d41d8cd98f00b204e9800998ecf8427e+0 0:0:file1.txt
`)
}

func (s *TestSuite) TestUploadError(c *C) {
	tmpdir, _ := ioutil.TempDir("", "")
	defer func() {
		os.RemoveAll(tmpdir)
	}()

	ioutil.WriteFile(tmpdir+"/"+"file1.txt", []byte("foo"), 0600)

	cw := CollectionWriter{0, &KeepErrorTestClient{}, nil, nil, sync.Mutex{}}
	str, err := cw.WriteTree(tmpdir, log.New(os.Stdout, "", 0))

	c.Check(err, NotNil)
	c.Check(str, Equals, "")
}
