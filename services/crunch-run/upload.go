// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

// Originally based on sdk/go/crunchrunner/upload.go
//
// Unlike the original, which iterates over a directory tree and uploads each
// file sequentially, this version supports opening and writing multiple files
// in a collection simultaneously.
//
// Eventually this should move into the Arvados Go SDK for a more comprehensive
// implementation of Collections.

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"git.curoverse.com/arvados.git/sdk/go/keepclient"
	"git.curoverse.com/arvados.git/sdk/go/manifest"
)

// Block is a data block in a manifest stream
type Block struct {
	data   []byte
	offset int64
}

// CollectionFileWriter is a Writer that permits writing to a file in a Keep Collection.
type CollectionFileWriter struct {
	IKeepClient
	*manifest.ManifestStream
	offset uint64
	length uint64
	*Block
	uploader chan *Block
	finish   chan []error
	fn       string
}

// Write to a file in a keep collection
func (m *CollectionFileWriter) Write(p []byte) (int, error) {
	n, err := m.ReadFrom(bytes.NewReader(p))
	return int(n), err
}

// ReadFrom a Reader and write to the Keep collection file.
func (m *CollectionFileWriter) ReadFrom(r io.Reader) (n int64, err error) {
	var total int64
	var count int

	for err == nil {
		if m.Block == nil {
			m.Block = &Block{make([]byte, keepclient.BLOCKSIZE), 0}
		}
		count, err = r.Read(m.Block.data[m.Block.offset:])
		total += int64(count)
		m.Block.offset += int64(count)
		if m.Block.offset == keepclient.BLOCKSIZE {
			m.uploader <- m.Block
			m.Block = nil
		}
	}

	m.length += uint64(total)

	if err == io.EOF {
		return total, nil
	}
	return total, err
}

// Close stops writing a file and adds it to the parent manifest.
func (m *CollectionFileWriter) Close() error {
	m.ManifestStream.FileStreamSegments = append(m.ManifestStream.FileStreamSegments,
		manifest.FileStreamSegment{m.offset, m.length, m.fn})
	return nil
}

func (m *CollectionFileWriter) NewFile(fn string) {
	m.offset += m.length
	m.length = 0
	m.fn = fn
}

func (m *CollectionFileWriter) goUpload(workers chan struct{}) {
	var mtx sync.Mutex
	var wg sync.WaitGroup

	var errors []error
	uploader := m.uploader
	finish := m.finish
	for block := range uploader {
		mtx.Lock()
		m.ManifestStream.Blocks = append(m.ManifestStream.Blocks, "")
		blockIndex := len(m.ManifestStream.Blocks) - 1
		mtx.Unlock()

		workers <- struct{}{} // wait for an available worker slot
		wg.Add(1)

		go func(block *Block, blockIndex int) {
			hash := fmt.Sprintf("%x", md5.Sum(block.data[0:block.offset]))
			signedHash, _, err := m.IKeepClient.PutHB(hash, block.data[0:block.offset])
			<-workers

			mtx.Lock()
			if err != nil {
				errors = append(errors, err)
			} else {
				m.ManifestStream.Blocks[blockIndex] = signedHash
			}
			mtx.Unlock()

			wg.Done()
		}(block, blockIndex)
	}
	wg.Wait()

	finish <- errors
}

// CollectionWriter implements creating new Keep collections by opening files
// and writing to them.
type CollectionWriter struct {
	MaxWriters int
	IKeepClient
	Streams []*CollectionFileWriter
	workers chan struct{}
	mtx     sync.Mutex
}

// Open a new file for writing in the Keep collection.
func (m *CollectionWriter) Open(path string) io.WriteCloser {
	var dir string
	var fn string

	i := strings.Index(path, "/")
	if i > -1 {
		dir = "./" + path[0:i]
		fn = path[i+1:]
	} else {
		dir = "."
		fn = path
	}

	fw := &CollectionFileWriter{
		m.IKeepClient,
		&manifest.ManifestStream{StreamName: dir},
		0,
		0,
		nil,
		make(chan *Block),
		make(chan []error),
		fn}

	m.mtx.Lock()
	defer m.mtx.Unlock()
	if m.workers == nil {
		if m.MaxWriters < 1 {
			m.MaxWriters = 2
		}
		m.workers = make(chan struct{}, m.MaxWriters)
	}

	go fw.goUpload(m.workers)

	m.Streams = append(m.Streams, fw)

	return fw
}

// Finish writing the collection, wait for all blocks to complete uploading.
func (m *CollectionWriter) Finish() error {
	var errstring string
	m.mtx.Lock()
	defer m.mtx.Unlock()

	for _, stream := range m.Streams {
		if stream.uploader == nil {
			continue
		}
		if stream.Block != nil {
			stream.uploader <- stream.Block
		}
		close(stream.uploader)
		stream.uploader = nil

		errors := <-stream.finish
		close(stream.finish)
		stream.finish = nil

		for _, r := range errors {
			errstring = fmt.Sprintf("%v%v\n", errstring, r.Error())
		}
	}
	if errstring != "" {
		return errors.New(errstring)
	}
	return nil
}

// ManifestText returns the manifest text of the collection.  Calls Finish()
// first to ensure that all blocks are written and that signed locators and
// available.
func (m *CollectionWriter) ManifestText() (mt string, err error) {
	err = m.Finish()
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer

	m.mtx.Lock()
	defer m.mtx.Unlock()
	for _, v := range m.Streams {
		if len(v.FileStreamSegments) == 0 {
			continue
		}
		k := v.StreamName
		if k == "." {
			buf.WriteString(".")
		} else {
			k = strings.Replace(k, " ", "\\040", -1)
			k = strings.Replace(k, "\n", "", -1)
			buf.WriteString("./" + k)
		}
		if len(v.Blocks) > 0 {
			for _, b := range v.Blocks {
				buf.WriteString(" ")
				buf.WriteString(b)
			}
		} else {
			buf.WriteString(" d41d8cd98f00b204e9800998ecf8427e+0")
		}
		for _, f := range v.FileStreamSegments {
			buf.WriteString(" ")
			name := strings.Replace(f.Name, " ", "\\040", -1)
			name = strings.Replace(name, "\n", "", -1)
			buf.WriteString(fmt.Sprintf("%v:%v:%v", f.SegPos, f.SegLen, name))
		}
		buf.WriteString("\n")
	}
	return buf.String(), nil
}

type WalkUpload struct {
	MaxWriters  int
	kc          IKeepClient
	stripPrefix string
	streamMap   map[string]*CollectionFileWriter
	status      *log.Logger
	workers     chan struct{}
	mtx         sync.Mutex
}

// WalkFunc walks a directory tree, uploads each file found and adds it to the
// CollectionWriter.
func (m *WalkUpload) WalkFunc(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	targetPath, targetInfo := path, info
	if info.Mode()&os.ModeSymlink != 0 {
		// Update targetpath/info to reflect the symlink
		// target, not the symlink itself
		targetPath, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		targetInfo, err = os.Stat(targetPath)
		if err != nil {
			return fmt.Errorf("stat symlink %q target %q: %s", path, targetPath, err)
		}
		if targetInfo.Mode()&os.ModeDir != 0 {
			// Symlinks to directories don't get walked, so do it
			// here.  We've previously checked that they stay in
			// the output directory and don't result in an endless
			// loop.
			var rd []os.FileInfo
			rd, err = ioutil.ReadDir(path)
			if err != nil {
				return err
			}
			for _, ent := range rd {
				err = filepath.Walk(filepath.Join(path, ent.Name()), m.WalkFunc)
			}
		}
	}

	if targetInfo.Mode()&os.ModeType != 0 {
		// Skip directories, pipes, other non-regular files
		return nil
	}

	var dir string
	if len(path) > (len(m.stripPrefix) + len(info.Name()) + 1) {
		dir = path[len(m.stripPrefix)+1 : (len(path) - len(info.Name()) - 1)]
	}
	if dir == "" {
		dir = "."
	}

	fn := path[(len(path) - len(info.Name())):]

	if m.streamMap[dir] == nil {
		m.streamMap[dir] = &CollectionFileWriter{
			m.kc,
			&manifest.ManifestStream{StreamName: dir},
			0,
			0,
			nil,
			make(chan *Block),
			make(chan []error),
			""}

		m.mtx.Lock()
		if m.workers == nil {
			if m.MaxWriters < 1 {
				m.MaxWriters = 2
			}
			m.workers = make(chan struct{}, m.MaxWriters)
		}
		m.mtx.Unlock()

		go m.streamMap[dir].goUpload(m.workers)
	}

	fileWriter := m.streamMap[dir]

	// Reset the CollectionFileWriter for a new file
	fileWriter.NewFile(fn)

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	m.status.Printf("Uploading %v/%v (%v bytes)", dir, fn, info.Size())

	_, err = io.Copy(fileWriter, file)
	if err != nil {
		return err
	}

	// Commits the current file.  Legal to call this repeatedly.
	fileWriter.Close()

	return nil
}

func (cw *CollectionWriter) WriteTree(root string, status *log.Logger) (manifest string, err error) {
	streamMap := make(map[string]*CollectionFileWriter)
	wu := &WalkUpload{0, cw.IKeepClient, root, streamMap, status, nil, sync.Mutex{}}
	err = filepath.Walk(root, wu.WalkFunc)

	if err != nil {
		return "", err
	}

	cw.mtx.Lock()
	for _, st := range streamMap {
		cw.Streams = append(cw.Streams, st)
	}
	cw.mtx.Unlock()

	return cw.ManifestText()
}
