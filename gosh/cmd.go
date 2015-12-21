// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gosh

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	errAlreadyCalledStart = errors.New("gosh: already called Cmd.Start")
	errAlreadyCalledWait  = errors.New("gosh: already called Cmd.Wait")
	errDidNotCallStart    = errors.New("gosh: did not call Cmd.Start")
)

// Cmd represents a command. Not thread-safe.
// Public fields should not be modified after calling Start.
type Cmd struct {
	// Err is the most recent error from this Cmd (may be nil).
	Err error
	// Path is the path of the command to run.
	Path string
	// Vars is the map of env vars for this Cmd.
	Vars map[string]string
	// Args is the list of args for this Cmd.
	Args []string
	// PropagateOutput is inherited from Shell.Opts.PropagateChildOutput.
	PropagateOutput bool
	// OutputDir is inherited from Shell.Opts.ChildOutputDir.
	OutputDir string
	// ExitErrorIsOk specifies whether an *exec.ExitError should be reported via
	// Shell.HandleError.
	ExitErrorIsOk bool
	// Stdin is a string to write to the child's stdin.
	Stdin string
	// Internal state.
	sh               *Shell
	c                *exec.Cmd
	stdinWriteCloser io.WriteCloser // from exec.Cmd.StdinPipe
	calledStart      bool
	calledWait       bool
	waitChan         chan error
	started          bool // protected by sh.cleanupMu
	exitedMu         sync.Mutex
	exited           bool // protected by exitedMu
	stdoutWriters    []io.Writer
	stderrWriters    []io.Writer
	closers          []io.Closer
	condReady        *sync.Cond
	recvReady        bool // protected by condReady.L
	condVars         *sync.Cond
	recvVars         map[string]string // protected by condVars.L
}

// Clone returns a new Cmd with a copy of this Cmd's configuration.
func (c *Cmd) Clone() *Cmd {
	c.sh.Ok()
	res, err := c.clone()
	c.handleError(err)
	return res
}

// StdinPipe returns a thread-safe WriteCloser backed by a buffered pipe for the
// command's stdin. The returned WriteCloser will be closed when the process
// exits. Must be called before Start. It is safe to call StdinPipe multiple
// times; calls after the first return the pipe created by the first call.
func (c *Cmd) StdinPipe() io.WriteCloser {
	c.sh.Ok()
	res, err := c.stdinPipe()
	c.handleError(err)
	return res
}

// StdoutPipe returns a Reader backed by a buffered pipe for the command's
// stdout. Must be called before Start. May be called more than once; each
// invocation creates a new pipe.
func (c *Cmd) StdoutPipe() io.Reader {
	c.sh.Ok()
	res, err := c.stdoutPipe()
	c.handleError(err)
	return res
}

// StderrPipe returns a Reader backed by a buffered pipe for the command's
// stderr. Must be called before Start. May be called more than once; each
// invocation creates a new pipe.
func (c *Cmd) StderrPipe() io.Reader {
	c.sh.Ok()
	res, err := c.stderrPipe()
	c.handleError(err)
	return res
}

// AddStdoutWriter configures this Cmd to tee the child's stdout to the given
// Writer. If this Writer is a Closer and is not os.Stdout or os.Stderr, it will
// be closed when the process exits.
func (c *Cmd) AddStdoutWriter(w io.Writer) {
	c.sh.Ok()
	c.handleError(c.addStdoutWriter(w))
}

// AddStderrWriter configures this Cmd to tee the child's stderr to the given
// Writer. If this Writer is a Closer and is not os.Stdout or os.Stderr, it will
// be closed when the process exits.
func (c *Cmd) AddStderrWriter(w io.Writer) {
	c.sh.Ok()
	c.handleError(c.addStderrWriter(w))
}

// Start starts the command.
func (c *Cmd) Start() {
	c.sh.Ok()
	c.handleError(c.start())
}

// AwaitReady waits for the child process to call SendReady. Must not be called
// before Start or after Wait.
func (c *Cmd) AwaitReady() {
	c.sh.Ok()
	c.handleError(c.awaitReady())
}

// AwaitVars waits for the child process to send values for the given vars
// (using SendVars). Must not be called before Start or after Wait.
func (c *Cmd) AwaitVars(keys ...string) map[string]string {
	c.sh.Ok()
	res, err := c.awaitVars(keys...)
	c.handleError(err)
	return res
}

// Wait waits for the command to exit.
func (c *Cmd) Wait() {
	c.sh.Ok()
	c.handleError(c.wait())
}

// TODO(sadovsky): Maybe add a method to send SIGINT, wait for a bit, then send
// SIGKILL if the process hasn't exited.

// Shutdown sends the given signal to the command, then waits for it to exit.
func (c *Cmd) Shutdown(sig os.Signal) {
	c.sh.Ok()
	c.handleError(c.shutdown(sig))
}

// Run calls Start followed by Wait.
func (c *Cmd) Run() {
	c.sh.Ok()
	c.handleError(c.run())
}

// Stdout calls Start followed by Wait, then returns the command's stdout.
func (c *Cmd) Stdout() string {
	c.sh.Ok()
	res, err := c.stdout()
	c.handleError(err)
	return res
}

// StdoutStderr calls Start followed by Wait, then returns the command's stdout
// and stderr.
func (c *Cmd) StdoutStderr() (string, string) {
	c.sh.Ok()
	stdout, stderr, err := c.stdoutStderr()
	c.handleError(err)
	return stdout, stderr
}

// Process returns the underlying process handle for the command.
func (c *Cmd) Process() *os.Process {
	c.sh.Ok()
	res, err := c.process()
	c.handleError(err)
	return res
}

////////////////////////////////////////
// Internals

func newCmdInternal(sh *Shell, vars map[string]string, path string, args []string) (*Cmd, error) {
	c := &Cmd{
		Path:      path,
		Vars:      vars,
		Args:      args,
		sh:        sh,
		c:         &exec.Cmd{},
		waitChan:  make(chan error, 1),
		condReady: sync.NewCond(&sync.Mutex{}),
		condVars:  sync.NewCond(&sync.Mutex{}),
		recvVars:  map[string]string{},
	}
	// Protect against concurrent signal-triggered Shell.cleanup().
	sh.cleanupMu.Lock()
	defer sh.cleanupMu.Unlock()
	if sh.calledCleanup {
		return nil, errAlreadyCalledCleanup
	}
	sh.cmds = append(sh.cmds, c)
	return c, nil
}

func newCmd(sh *Shell, vars map[string]string, name string, args ...string) (*Cmd, error) {
	// Mimics https://golang.org/src/os/exec/exec.go Command.
	if filepath.Base(name) == name {
		if lp, err := exec.LookPath(name); err != nil {
			return nil, err
		} else {
			name = lp
		}
	}
	return newCmdInternal(sh, vars, name, args)
}

func (c *Cmd) errorIsOk(err error) bool {
	if c.ExitErrorIsOk {
		if _, ok := err.(*exec.ExitError); ok {
			return true
		}
	}
	return err == nil
}

func (c *Cmd) handleError(err error) {
	c.Err = err
	if !c.errorIsOk(err) {
		c.sh.HandleError(err)
	}
}

func (c *Cmd) addWriter(writers *[]io.Writer, w io.Writer) {
	*writers = append(*writers, w)
	// Check for os.Stdout and os.Stderr so that we don't close these when a
	// single command exits. This technique isn't foolproof (since, for example,
	// os.Stdout may be wrapped in another WriteCloser), but in practice it should
	// be adequate.
	if w != os.Stdout && w != os.Stderr {
		if wc, ok := w.(io.Closer); ok {
			c.closers = append(c.closers, wc)
		}
	}
}

func (c *Cmd) closeClosers() {
	for _, c := range c.closers {
		c.Close() // best-effort; ignore returned error
	}
}

func (c *Cmd) isRunning() bool {
	if !c.started {
		return false
	}
	c.exitedMu.Lock()
	defer c.exitedMu.Unlock()
	return !c.exited
}

// recvWriter listens for gosh messages from a child process.
type recvWriter struct {
	c          *Cmd
	buf        bytes.Buffer
	readPrefix bool // if true, we've read len(msgPrefix) for the current line
	skipLine   bool // if true, ignore bytes until next '\n'
}

func (w *recvWriter) Write(p []byte) (n int, err error) {
	for _, b := range p {
		if b == '\n' {
			if w.readPrefix && !w.skipLine {
				m := msg{}
				if err := json.Unmarshal(w.buf.Bytes(), &m); err != nil {
					return 0, err
				}
				switch m.Type {
				case typeReady:
					w.c.condReady.L.Lock()
					w.c.recvReady = true
					w.c.condReady.Signal()
					w.c.condReady.L.Unlock()
				case typeVars:
					w.c.condVars.L.Lock()
					w.c.recvVars = mergeMaps(w.c.recvVars, m.Vars)
					w.c.condVars.Signal()
					w.c.condVars.L.Unlock()
				default:
					return 0, fmt.Errorf("unknown message type: %q", m.Type)
				}
			}
			// Reset state for next line.
			w.readPrefix, w.skipLine = false, false
			w.buf.Reset()
		} else if !w.skipLine {
			w.buf.WriteByte(b)
			if !w.readPrefix && w.buf.Len() == len(msgPrefix) {
				w.readPrefix = true
				prefix := string(w.buf.Next(len(msgPrefix)))
				if prefix != msgPrefix {
					w.skipLine = true
				}
			}
		}
	}
	return len(p), nil
}

func (c *Cmd) initMultiWriter(f *os.File, t string) (io.Writer, error) {
	var writers *[]io.Writer
	if f == os.Stdout {
		writers = &c.stdoutWriters
		c.addWriter(writers, &recvWriter{c: c})
	} else {
		writers = &c.stderrWriters
	}
	if c.PropagateOutput {
		c.addWriter(writers, f)
	}
	if c.OutputDir != "" {
		suffix := "stderr"
		if f == os.Stdout {
			suffix = "stdout"
		}
		name := filepath.Join(c.OutputDir, filepath.Base(c.Path)+"."+t+"."+suffix)
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		c.addWriter(writers, file)
	}
	return io.MultiWriter(*writers...), nil
}

func (c *Cmd) clone() (*Cmd, error) {
	vars := make(map[string]string, len(c.Vars))
	for k, v := range c.Vars {
		vars[k] = v
	}
	args := make([]string, len(c.Args))
	copy(args, c.Args)
	res, err := newCmdInternal(c.sh, vars, c.Path, args)
	if err != nil {
		return nil, err
	}
	res.PropagateOutput = c.PropagateOutput
	res.OutputDir = c.OutputDir
	res.ExitErrorIsOk = c.ExitErrorIsOk
	res.Stdin = c.Stdin
	return res, nil
}

func (c *Cmd) stdinPipe() (io.WriteCloser, error) {
	if c.calledStart {
		return nil, errAlreadyCalledStart
	}
	if c.stdinWriteCloser != nil {
		return c.stdinWriteCloser, nil
	}
	var err error
	c.stdinWriteCloser, err = c.c.StdinPipe()
	return c.stdinWriteCloser, err
}

func (c *Cmd) stdoutPipe() (io.Reader, error) {
	if c.calledStart {
		return nil, errAlreadyCalledStart
	}
	p := NewBufferedPipe()
	c.addWriter(&c.stdoutWriters, p)
	return p, nil
}

func (c *Cmd) stderrPipe() (io.Reader, error) {
	if c.calledStart {
		return nil, errAlreadyCalledStart
	}
	p := NewBufferedPipe()
	c.addWriter(&c.stderrWriters, p)
	return p, nil
}

func (c *Cmd) addStdoutWriter(w io.Writer) error {
	if c.calledStart {
		return errAlreadyCalledStart
	}
	c.addWriter(&c.stdoutWriters, w)
	return nil
}

func (c *Cmd) addStderrWriter(w io.Writer) error {
	if c.calledStart {
		return errAlreadyCalledStart
	}
	c.addWriter(&c.stderrWriters, w)
	return nil
}

// TODO(sadovsky): Maybe wrap every child process with a "supervisor" process
// that calls WatchParent().

func (c *Cmd) start() error {
	if c.calledStart {
		return errAlreadyCalledStart
	}
	c.calledStart = true
	// Protect against Cmd.start() writing to c.c.Process concurrently with
	// signal-triggered Shell.cleanup() reading from it.
	c.sh.cleanupMu.Lock()
	defer c.sh.cleanupMu.Unlock()
	if c.sh.calledCleanup {
		return errAlreadyCalledCleanup
	}
	// Configure the command.
	c.c.Path = c.Path
	c.c.Env = mapToSlice(c.Vars)
	c.c.Args = append([]string{c.Path}, c.Args...)
	if c.Stdin != "" {
		if c.stdinWriteCloser != nil {
			return errors.New("gosh: cannot both set Stdin and call StdinPipe")
		}
		c.c.Stdin = strings.NewReader(c.Stdin)
	}
	t := time.Now().Format("20060102.150405.000000")
	var err error
	if c.c.Stdout, err = c.initMultiWriter(os.Stdout, t); err != nil {
		return err
	}
	if c.c.Stderr, err = c.initMultiWriter(os.Stderr, t); err != nil {
		return err
	}
	// Start the command.
	err = c.c.Start()
	if err != nil {
		c.exitedMu.Lock()
		c.exited = true
		c.exitedMu.Unlock()
		c.closeClosers()
		c.waitChan <- errors.New("gosh: start failed")
		return err
	}
	c.started = true
	// Spawn a "waiter" goroutine that calls exec.Cmd.Wait and thus waits for the
	// process to exit. Calling exec.Cmd.Wait here rather than in gosh.Cmd.Wait
	// ensures that the child process is reaped once it exits. Note, gosh.Cmd.wait
	// blocks on waitChan.
	go func() {
		err := c.c.Wait()
		c.exitedMu.Lock()
		c.exited = true
		c.exitedMu.Unlock()
		c.closeClosers()
		c.waitChan <- err
	}()
	return nil
}

// TODO(sadovsky): Make it so Cmd.{awaitReady,awaitVars} return an error if/when
// we detect that the process has exited. Also, maybe add optional timeouts for
// Cmd.{awaitReady,awaitVars,wait}.

func (c *Cmd) awaitReady() error {
	if !c.started {
		return errDidNotCallStart
	} else if c.calledWait {
		return errAlreadyCalledWait
	}
	// http://golang.org/pkg/sync/#Cond.Wait
	c.condReady.L.Lock()
	for !c.recvReady {
		c.condReady.Wait()
	}
	c.condReady.L.Unlock()
	return nil
}

func (c *Cmd) awaitVars(keys ...string) (map[string]string, error) {
	if !c.started {
		return nil, errDidNotCallStart
	} else if c.calledWait {
		return nil, errAlreadyCalledWait
	}
	wantKeys := map[string]bool{}
	for _, key := range keys {
		wantKeys[key] = true
	}
	res := map[string]string{}
	updateRes := func() {
		for k, v := range c.recvVars {
			if _, ok := wantKeys[k]; ok {
				res[k] = v
			}
		}
	}
	// http://golang.org/pkg/sync/#Cond.Wait
	c.condVars.L.Lock()
	updateRes()
	for len(res) < len(wantKeys) {
		c.condVars.Wait()
		updateRes()
	}
	c.condVars.L.Unlock()
	return res, nil
}

func (c *Cmd) wait() error {
	if !c.started {
		return errDidNotCallStart
	} else if c.calledWait {
		return errAlreadyCalledWait
	}
	c.calledWait = true
	return <-c.waitChan
}

func (c *Cmd) shutdown(sig os.Signal) error {
	if !c.started {
		return errDidNotCallStart
	}
	// TODO(sadovsky): There's a race condition here and in
	// Shell.terminateRunningCmds. If our Process.Wait returns immediately before
	// we call Process.Signal, Process.Signal will return an error, "os: process
	// already finished". Should we add Cmd.Signal and Cmd.Kill methods that
	// special-case for this error message?
	if !c.isRunning() {
		return nil
	}
	if err := c.c.Process.Signal(sig); err != nil {
		return err
	}
	if err := c.wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return err
		}
	}
	return nil
}

func (c *Cmd) run() error {
	if err := c.start(); err != nil {
		return err
	}
	return c.wait()
}

func (c *Cmd) stdout() (string, error) {
	if c.calledStart {
		return "", errAlreadyCalledStart
	}
	var stdout bytes.Buffer
	c.addWriter(&c.stdoutWriters, &stdout)
	err := c.run()
	return stdout.String(), err
}

func (c *Cmd) stdoutStderr() (string, string, error) {
	if c.calledStart {
		return "", "", errAlreadyCalledStart
	}
	var stdout, stderr bytes.Buffer
	c.addWriter(&c.stdoutWriters, &stdout)
	c.addWriter(&c.stderrWriters, &stderr)
	err := c.run()
	return stdout.String(), stderr.String(), err
}

func (c *Cmd) process() (*os.Process, error) {
	if !c.started {
		return nil, errDidNotCallStart
	}
	return c.c.Process, nil
}