// Copyright 2016 Yeung Shu Hung and The Go Authors.
// All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements the web server side for FastCGI
// as specified in http://www.mit.edu/~yandros/doc/specs/fcgi-spec.html

// A part of this file is from golang package net/http/cgi,
// in particular https://golang.org/src/net/http/cgi/host.go

package gofast

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Role for fastcgi application in spec
type Role uint16

// Roles specified in the fastcgi spec
const (
	RoleResponder Role = iota + 1
	RoleAuthorizer
	RoleFilter
)

// NewRequest returns a standard FastCGI request
// with a unique request ID allocted by the client
func NewRequest(r *http.Request) (req *Request) {
	req = &Request{
		Raw:    r,
		Role:   RoleResponder,
		Params: make(map[string]string),
	}

	// if no http request, return here
	if r == nil {
		return
	}

	// pass body (io.ReadCloser) to stdio
	req.Stdin = r.Body
	return
}

// Request hold information of a standard
// FastCGI request
type Request struct {
	Raw      *http.Request
	Role     Role
	Params   map[string]string
	Stdin    io.ReadCloser
	Data     io.ReadCloser
	KeepConn bool
}

type idPool struct {
	IDs chan uint16
}

// AllocID implements Client.AllocID
func (p *idPool) Alloc() uint16 {
	return <-p.IDs
}

// ReleaseID implements Client.ReleaseID
func (p *idPool) Release(id uint16) {
	go func() {
		// release the ID back to channel for reuse
		// use goroutine to prev0, ent blocking ReleaseID
		p.IDs <- id
	}()
}

func newIDs(limit uint32) (p idPool) {

	// sanatize limit
	if limit == 0 || limit > 65536 {
		limit = 65536
	}

	// pool requestID for the client
	//
	// requestID: Identifies the FastCGI request to which the record belongs.
	// The Web server re-uses FastCGI request IDs; the application
	// keeps track of the current state of each request ID on a given
	// transport connection.
	//
	// Ref: https://fast-cgi.github.io/spec#33-records
	ids := make(chan uint16)
	go func(maxID uint16) {
		for i := uint16(0); i < maxID; i++ {
			ids <- i
		}
		ids <- uint16(maxID)
	}(uint16(limit - 1))

	p.IDs = ids
	return
}

// client is the default implementation of Client
type client struct {
	conn *conn
	ids  idPool
}

// writeRequest writes params and stdin to the FastCGI application
func (c *client) writeRequest(reqID uint16, req *Request) (err error) {

	// end request whenever the function block ends
	defer func() {
		if err != nil {
			// abort the request if there is any error
			// in previous request writing process.
			c.conn.writeAbortRequest(reqID)
			return
		}
	}()

	// write request header with specified role
	err = c.conn.writeBeginRequest(reqID, req.Role, 1)
	if err != nil {
		return
	}
	err = c.conn.writePairs(typeParams, reqID, req.Params)
	if err != nil {
		return
	}

	// write the stdin stream
	stdinWriter := newWriter(c.conn, typeStdin, reqID)
	if req.Stdin != nil {
		defer req.Stdin.Close()
		p := make([]byte, 1024)
		var count int
		for {
			count, err = req.Stdin.Read(p)
			if err == io.EOF {
				err = nil
			} else if err != nil {
				stdinWriter.Close()
				return
			}
			if count == 0 {
				break
			}

			_, err = stdinWriter.Write(p[:count])
			if err != nil {
				stdinWriter.Close()
				return
			}
		}
	}
	if err = stdinWriter.Close(); err != nil {
		return
	}

	// for filter role, also add the data stream
	if req.Role == RoleFilter {
		// write the data stream
		dataWriter := newWriter(c.conn, typeData, reqID)
		defer req.Data.Close()
		p := make([]byte, 1024)
		var count int
		for {
			count, err = req.Data.Read(p)
			if err == io.EOF {
				err = nil
			} else if err != nil {
				return
			}
			if count == 0 {
				break
			}

			_, err = dataWriter.Write(p[:count])
			if err != nil {
				return
			}
		}
		if err = dataWriter.Close(); err != nil {
			return
		}
	}
	return
}

// readResponse read the FastCGI stdout and stderr, then write
// to the response pipe. Protocol error will also be written
// to the error writer in ResponsePipe.
func (c *client) readResponse(ctx context.Context, resp *ResponsePipe, req *Request) {

	var rec record
	done := make(chan int)

	// readloop in goroutine
	go func() {
	readLoop:
		for {
			if err := rec.read(c.conn.rwc); err != nil {
				break
			}

			// different output type for different stream
			switch rec.h.Type {
			case typeStdout:
				resp.stdOutWriter.Write(rec.content())
			case typeStderr:
				resp.stdErrWriter.Write(rec.content())
			case typeEndRequest:
				break readLoop
			default:
				err := fmt.Sprintf("unexpected type %#v in readLoop", rec.h.Type)
				resp.stdErrWriter.Write([]byte(err))
			}
		}
		close(done)
	}()

	select {
	case <-ctx.Done():
		// do nothing, let client.Do handle
	case <-done:
		// do nothing and end the function
	}
	return
}

// Do implements Client.Do
func (c *client) Do(req *Request) (resp *ResponsePipe, err error) {

	// validate the request
	// if role is a filter, it has to have Data stream
	if req.Role == RoleFilter {
		// validate the request
		if req.Data == nil {
			err = fmt.Errorf("filter request requries a data stream")
		} else if _, ok := req.Params["FCGI_DATA_LAST_MOD"]; !ok {
			err = fmt.Errorf("filter request requries param FCGI_DATA_LAST_MOD")
		} else if _, err = strconv.ParseUint(req.Params["FCGI_DATA_LAST_MOD"], 10, 32); err != nil {
			err = fmt.Errorf("invalid parsing FCGI_DATA_LAST_MOD (%s)", err)
		} else if _, ok := req.Params["FCGI_DATA_LENGTH"]; !ok {
			err = fmt.Errorf("filter request requries param FCGI_DATA_LENGTH")
		} else if _, err = strconv.ParseUint(req.Params["FCGI_DATA_LENGTH"], 10, 32); err != nil {
			err = fmt.Errorf("invalid parsing FCGI_DATA_LENGTH (%s)", err)
		}

		// if invalid, end the response stream and return
		if err != nil {
			return
		}
	}

	// allocate request ID
	reqID := c.ids.Alloc()

	// create response pipe
	resp = NewResponsePipe()
	writeError, allDone := make(chan error), make(chan int)

	// check if connection exists
	if c.conn == nil {
		err = fmt.Errorf("client connection has been closed")
		return
	}

	// if there is a raw request, use the context deadline
	var ctx context.Context
	if req.Raw != nil {
		ctx = req.Raw.Context()
	} else {
		ctx = context.TODO()
	}

	// wait group to wait for both read and write to end
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		wg.Wait()
		close(allDone)
	}()

	// Run read and write in parallel.
	// Note: Specification never said "write before read".
	go func() {
		if err := c.writeRequest(reqID, req); err != nil {
			writeError <- err
		}
		wg.Done()
	}()

	// get response in a goroutine and send to response pipe
	go func() {
		c.readResponse(ctx, resp, req)
		wg.Done()
	}()

	// do not block the return of client.Do
	// and return the response pipes
	// (or else would be block by the response pipes not being used)
	go func() {
		// wait until context deadline
		// or until writeError is not blocked.
		select {
		case <-ctx.Done():
			// write error to err writer
			resp.stdErrWriter.Write([]byte("gofast: timeout or canceled"))
		case err := <-writeError:
			// write error to err writer
			resp.stdErrWriter.Write([]byte(err.Error()))
		case <-allDone:
			// do nothing
		}

		// clean up
		c.ids.Release(reqID)
		resp.Close()
		close(writeError)
	}()
	return
}

// Close implements Client.Close
// If the inner connection has been closed before,
// this method would do nothing and return nil
func (c *client) Close() (err error) {
	if c.conn == nil {
		return
	}
	err = c.conn.Close()
	c.conn = nil
	return
}

// Client is a client interface of FastCGI
// application process through given
// connection (net.Conn)
type Client interface {

	// Do  a proper FastCGI request.
	// Returns the response streams (stdout and stderr)
	// and the request validation error.
	//
	// Note: protocol error will be written to the stderr
	// stream in the ResponsePipe.
	Do(req *Request) (resp *ResponsePipe, err error)

	// Close the underlying connection
	Close() error
}

// ConnFactory creates new network connections
// to the FPM application
type ConnFactory func() (net.Conn, error)

// SimpleConnFactory creates the simplest ConnFactory implementation.
func SimpleConnFactory(network, address string) ConnFactory {
	return func() (net.Conn, error) {
		return net.Dial(network, address)
	}
}

// ClientFactory creates new FPM client with proper connection
// to the FPM application.
type ClientFactory func() (Client, error)

// SimpleClientFactory returns a ClientFactory implementation
// with the given ConnFactory.
//
// limit is the maximum number of request that the
// applcation support. 0 means the maximum number
// available for 16bit request id (65536).
// Default 0.
//
func SimpleClientFactory(connFactory ConnFactory, limit uint32) ClientFactory {
	return func() (c Client, err error) {
		// connect to given network address
		conn, err := connFactory()
		if err != nil {
			return
		}

		// create client
		c = &client{
			conn: newConn(conn),
			ids:  newIDs(limit),
		}
		return
	}
}

// NewResponsePipe returns an initialized new ResponsePipe struct
func NewResponsePipe() (p *ResponsePipe) {
	p = new(ResponsePipe)
	p.stdOutReader, p.stdOutWriter = io.Pipe()
	p.stdErrReader, p.stdErrWriter = io.Pipe()
	return
}

func readHeader(linebody *bufio.Reader) (statusCode int, header http.Header, err error) {

	header = make(http.Header)
	headerLines, sawBlankLine := 0, false
	for {
		var line []byte
		var isPrefix bool
		line, isPrefix, err = linebody.ReadLine()
		if isPrefix {
			statusCode = http.StatusBadGateway
			err = fmt.Errorf("gofast: long header line from subprocess")
			return
		}
		if err != nil {
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			statusCode = http.StatusBadGateway
			err = fmt.Errorf("gofast: error reading headers: %v", err)
			return
		}
		if len(line) == 0 {
			sawBlankLine = true
			break
		}
		headerLines++
		parts := strings.SplitN(string(line), ":", 2)
		if len(parts) < 2 {
			err = fmt.Errorf("gofast: bogus header line: %s", string(line))
			return
		}

		key, val := parts[0], parts[1]
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "Status":
			if len(val) < 3 {
				err = fmt.Errorf("gofast: bogus status (short): %q", val)
				return
			}
			var code int
			code, err = strconv.Atoi(val[0:3])
			if err != nil {
				err = fmt.Errorf("gofast: bogus status: %q\nline was %q",
					val, line)
				return
			}
			statusCode = code
		default:
			header.Add(key, val)
		}
	}
	if headerLines == 0 || !sawBlankLine {
		statusCode = http.StatusBadGateway
		err = fmt.Errorf("gofast: no headers")
		return
	}
	return
}

func writeHeader(w http.ResponseWriter, statusCode int, header http.Header) (err error) {

	// validate header
	if header.Get("Content-Type") == "" {
		statusCode = http.StatusBadGateway
		err = fmt.Errorf("gofast: missing required Content-Type in headers")
		return
	}

	// infer status code, if not given one
	if statusCode == 0 {
		// try to resolve statusCode from header hints
		if loc := header.Get("Location"); loc != "" {
			statusCode = http.StatusFound
		} else {
			statusCode = http.StatusOK
		}
	}

	// Copy headers to rw's headers, after we've decided not to
	// go into handleInternalRedirect, which won't want its rw
	// headers to have been touched.
	for k, vv := range header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	// start writing response
	w.WriteHeader(statusCode)
	return
}

// ResponsePipe contains readers and writers that handles
// all FastCGI output streams
type ResponsePipe struct {
	stdOutReader io.Reader
	stdOutWriter io.WriteCloser
	stdErrReader io.Reader
	stdErrWriter io.WriteCloser
}

// Close close all writers
func (pipes *ResponsePipe) Close() {
	pipes.stdOutWriter.Close()
	pipes.stdErrWriter.Close()
}

// WriteTo writes the given output into http.ResponseWriter
func (pipes *ResponsePipe) WriteTo(rw http.ResponseWriter, ew io.Writer) (err error) {
	chErr := make(chan error, 2)
	defer close(chErr)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		chErr <- pipes.writeResponse(rw)
		wg.Done()
	}()
	go func() {
		chErr <- pipes.writeError(ew)
		wg.Done()
	}()

	wg.Wait()
	for i := 0; i < 2; i++ {
		if err = <-chErr; err != nil {
			return
		}
	}
	return
}

func (pipes *ResponsePipe) writeError(w io.Writer) (err error) {
	_, err = io.Copy(w, pipes.stdErrReader)
	if err != nil {
		err = fmt.Errorf("gofast: copy error: %v", err.Error())
	}
	return
}

// writeTo writes the given output into http.ResponseWriter
func (pipes *ResponsePipe) writeResponse(w http.ResponseWriter) (err error) {

	linebody := bufio.NewReaderSize(pipes.stdOutReader, 1024)
	statusCode, header, err := readHeader(linebody)
	if err != nil {
		return
	}

	// write header to writer
	writeHeader(w, statusCode, header)

	// write body to writer
	_, err = io.Copy(w, linebody)
	if err != nil {
		err = fmt.Errorf("gofast: copy error: %v", err)
	}
	return
}
