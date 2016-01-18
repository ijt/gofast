// Copyright 2016 Yeung Shu Hung and The Go Authors.
// All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements the web server side for FastCGI
// as specified in http://www.fastcgi.com/drupal/node/22

// A part of this file is from golang package net/http/cgi,
// in particular https://golang.org/src/net/http/cgi/host.go

package gofast

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// client is the default implementation of Client
type client struct {
	conn *conn

	chanID chan uint16
	ids    map[uint16]bool
}

// AllocID implements Client.AllocID
func (c *client) AllocID() (reqID uint16) {
	for {
		reqID = <-c.chanID
		if c.ids[reqID] != true {
			break
		}
	}
	c.ids[reqID] = true
	return
}

// ReleaseID implements Client.ReleaseID
func (c *client) ReleaseID(reqID uint16) {
	c.ids[reqID] = false
	go func() {
		// release the ID back to channel for reuse
		// use goroutine to prevent blocking ReleaseID
		c.chanID <- reqID
	}()
}

// Handle implements Client.Handle
func (c *client) Handle(w *ResponsePipe, req *Request) (err error) {
	defer c.ReleaseID(req.GetID())
	defer w.StdOutWriter.Close()

	err = c.conn.writeBeginRequest(req.GetID(), uint16(roleResponder), 0)
	if err != nil {
		return
	}
	err = c.conn.writePairs(typeParams, req.GetID(), req.Params)
	if err != nil {
		return
	}
	if len(req.Content) > 0 {
		err = c.conn.writeRecord(typeStdin, req.GetID(), req.Content)
		if err != nil {
			return
		}
	}

	var rec record

readLoop:
	for {
		if err := rec.read(c.conn.rwc); err != nil {
			break
		}

		// different output type for different stream
		switch rec.h.Type {
		case typeStdout:
			w.StdOutWriter.Write(rec.content())
		case typeStderr:
			w.StdErrBuffer.Write(rec.content())
		case typeEndRequest:
			break readLoop
		default:
			panic(fmt.Sprintf("unexpected type %#v in readLoop", rec.h.Type))
		}
	}

	return
}

// NewRequest implements Client.NewRequest
func (c *client) NewRequest() *Request {
	return &Request{
		ID:     c.AllocID(),
		Params: make(map[string]string),
	}
}

// Client is a client interface of FastCGI
// application process through given
// connection (net.Conn)
type Client interface {

	// Handle takes care of a proper FastCGI request
	Handle(pipes *ResponsePipe, req *Request) (err error)

	// NewRequest returns a standard FastCGI request
	// with a unique request ID allocted by the client
	NewRequest() *Request

	// AllocID allocates a new reqID.
	// It blocks if all possible uint16 IDs are allocated.
	AllocID() uint16

	// ReleaseID releases a reqID.
	// It never blocks.
	ReleaseID(uint16)
}

// NewClient returns a Client of the given
// connection (net.Conn)
func NewClient(conn net.Conn) Client {
	cid := make(chan uint16)
	go func() {
		for i := uint16(0); i < 65535; i++ {
			cid <- i
		}
		cid <- uint16(65535)
	}()

	return &client{
		conn:   newConn(conn),
		ids:    make(map[uint16]bool),
		chanID: cid,
	}
}

// NewResponsePipe returns an initialized new ResponsePipe struct
func NewResponsePipe() (p *ResponsePipe) {
	p = new(ResponsePipe)
	p.StdOutReader, p.StdOutWriter = io.Pipe()
	p.StdErrBuffer = new(bytes.Buffer)
	return
}

// ResponsePipe contains readers and writers that handles
// all FastCGI output streams
type ResponsePipe struct {
	StdOutReader io.Reader
	StdOutWriter io.WriteCloser
	StdErrBuffer *bytes.Buffer
}

// WriteTo writes the given output into http.ResponseWriter
func (pipes *ResponsePipe) WriteTo(w http.ResponseWriter) {
	linebody := bufio.NewReaderSize(pipes.StdOutReader, 1024)
	headers := make(http.Header)
	statusCode := 0
	headerLines := 0
	sawBlankLine := false

	for {
		line, isPrefix, err := linebody.ReadLine()
		if isPrefix {
			w.WriteHeader(http.StatusInternalServerError)
			log.Printf("gofast: long header line from subprocess.")
			return
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			log.Printf("gofast: error reading headers: %v", err)
			return
		}
		if len(line) == 0 {
			sawBlankLine = true
			break
		}
		headerLines++
		parts := strings.SplitN(string(line), ":", 2)
		if len(parts) < 2 {
			log.Printf("gofast: bogus header line: %s", string(line))
			continue
		}
		header, val := parts[0], parts[1]
		header = strings.TrimSpace(header)
		val = strings.TrimSpace(val)
		switch {
		case header == "Status":
			if len(val) < 3 {
				log.Printf("gofast: bogus status (short): %q", val)
				return
			}
			code, err := strconv.Atoi(val[0:3])
			if err != nil {
				log.Printf("gofast: bogus status: %q", val)
				log.Printf("gofast: line was %q", line)
				return
			}
			statusCode = code
		default:
			headers.Add(header, val)
		}
	}
	if headerLines == 0 || !sawBlankLine {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("gofast: no headers")
		return
	}

	if loc := headers.Get("Location"); loc != "" {
		/*
			if strings.HasPrefix(loc, "/") && h.PathLocationHandler != nil {
				h.handleInternalRedirect(rw, req, loc)
				return
			}
		*/
		if statusCode == 0 {
			statusCode = http.StatusFound
		}
	}

	if statusCode == 0 && headers.Get("Content-Type") == "" {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("gofast: missing required Content-Type in headers")
		return
	}

	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	// Copy headers to rw's headers, after we've decided not to
	// go into handleInternalRedirect, which won't want its rw
	// headers to have been touched.
	for k, vv := range headers {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(statusCode)

	_, err := io.Copy(w, linebody)
	if err != nil {
		log.Printf("gofast: copy error: %v", err)
	}

}
