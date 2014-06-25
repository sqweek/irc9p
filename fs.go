package main

import (
	"github.com/sqweek/irc9p/irc"
	"code.google.com/p/go9p/p"
	"code.google.com/p/go9p/p/srv"
	"strings"
	"bytes"
	"errors"
	"log"
	"net"
	"os/signal"
	"syscall"
	"os"
	"io"
)

type File struct {
	srv.File
	name string
	mode uint32
}

type LineFile struct {
	File
	rdaux map[*srv.Fid] *ReadAux
	wraux map[*srv.Fid] *bytes.Buffer
	rdinit ReadInitFn
	wrline WriteLineFn
}

type ReadAux struct {
	buf *bytes.Buffer
	lines chan string
}

func NewReadAux(data string, lines chan string) *ReadAux {
	return &ReadAux{bytes.NewBufferString(data), lines}
}

type WriteLineFn func(string) error

type ReadInitFn func() *ReadAux

func (f *LineFile) Open(fid *srv.FFid, mode uint8) error {
	if mode & 3 == p.OREAD || mode & 3 == p.ORDWR {
		_, ok := f.rdaux[fid.Fid]
		if ok {
			return srv.Ebaduse
		}
		f.rdaux[fid.Fid] = f.rdinit()
	}
	return nil
}

func (f *LineFile) FidDestroy(fid *srv.FFid) {
	err := f.Clunk(fid)
	if err != nil {
		log.Println("Clunk error during FidDestroy: ", err)
	}
}

func (f *LineFile) Clunk(fid *srv.FFid) error {
	delete(f.rdaux, fid.Fid)
	buf, ok := f.wraux[fid.Fid]
	if ok && buf.Len() > 0 {
		err := dowrite(buf, f.wrline, true)
		if err != nil {
			return err
		}
	}
	delete(f.wraux, fid.Fid)
	return nil
}

func (f *LineFile) Read(fid *srv.FFid, data []byte, offset uint64) (int, error) {
	if f.rdinit == nil {
		return 0, srv.Eperm
	}
	n := 0
	rdaux, ok := f.rdaux[fid.Fid]
	if !ok {
		return 0, srv.Ebaduse
	}
	/* fill from existing buffer before blocking */
	if rdaux.buf.Len() > 0 {
		n, _ = rdaux.buf.Read(data)
		if n == len(data) {
			return n, nil
		}
	}
	line, ok := <-rdaux.lines
	if !ok {
		return n, nil
	} else if n + len(line) > len(data) {
		rdaux.buf.WriteString(line)
		n2, _ := rdaux.buf.Read(data[n:])
		return n + n2, nil
	} else {
		n2 := copy(data[n:], line)
		return n + n2, nil
	}
}

func (f *LineFile) Write(fid *srv.FFid, data []byte, offset uint64) (int, error) {
	if f.wrline == nil {
		return 0, srv.Eperm
	}
	buf, ok := f.wraux[fid.Fid]
	if !ok {
		buf = bytes.NewBuffer(data)
		f.wraux[fid.Fid] = buf
	} else {
		_, err := buf.Write(data)
		if err != nil {
			return 0, err
		}
	}
	err := dowrite(buf, f.wrline, false)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func dowrite(buf *bytes.Buffer, callback func(string) error, eof bool) error {
	for {
		line, err := buf.ReadString('\n')
		if err == io.EOF {
			if eof {
				callback(line)
			} else {
				buf.WriteString(line)
			}
			return nil
		}
		err = callback(line)
		if err != nil {
			return err
		}
	}
}

var uid p.User
var gid p.Group

type IrcFsRoot struct {
	irc *irc.Conn
	content chan irc.Content
	chans map[string] *IrcFsChan

	dir *srv.File
	ctl *LineFile
	event *LineFile
	nick *LineFile
	pong *LineFile
}

var root IrcFsRoot

type IrcFsChan struct {
	incoming chan string
	name string

	dir *srv.File
	ctl *LineFile
	data *LineFile
	users *LineFile
}

func newLineFile(name string, mode uint32, rdinit ReadInitFn, wrline WriteLineFn) *LineFile {
	var rdaux map[*srv.Fid] *ReadAux
	var wraux map[*srv.Fid] *bytes.Buffer
	if mode & 0400 != 0 {
		rdaux = make(map[*srv.Fid] *ReadAux)
	}
	if mode & 0200 != 0 {
		wraux = make(map[*srv.Fid] *bytes.Buffer)
	}
	return &LineFile{File{srv.File{}, name, mode}, rdaux, wraux, rdinit, wrline}
}

func add(parent *srv.File, children ...*LineFile) error {
	for _, child := range(children) {
		err := child.Add(parent, child.name, uid, gid, child.mode, child)
		if err != nil {
			return err
		}
	}
	return nil
}

/* Adds a channel directory and ctl/data files under the given parent dir */
func (ch *IrcFsChan) Add(parent *srv.File) error {
	err := ch.dir.Add(parent, ch.name, uid, gid, p.DMDIR|0777, nil)
	if err != nil {
		return err
	}
	err = add(ch.dir, ch.ctl, ch.data, ch.users)
	if err != nil {
		return err
	}
	return nil
}

func newFsChan(channel string) *IrcFsChan {
	return &IrcFsChan{
		make(chan string),
		channel,
		new(srv.File),
		newLineFile("ctl", 0222, nil, wrChanCtlFn(channel)),
		newLineFile("data", 0666, rdChanDataFn(channel), wrChanDataFn(channel)),
		newLineFile("users", 0444, rdChanUsersFn(channel), nil),
	}
}

func (root *IrcFsRoot) dispatch() {
	for content := range root.content {
		if len(content.Channel) == 0 {
			/* TODO empty channel => server message, dispatch differently */
			log.Println("server msg:", content.Data)
		} else {
			root.channel(content.Channel).incoming <- content.Data
		}
	}
}

func (root *IrcFsRoot) channel(name string) *IrcFsChan {
	c, ok := root.chans[name]
	if !ok {
		c = newFsChan(name)
		c.Add(root.dir)
		go c.chanDispatch()
		root.chans[name] = c
	}
	return c
}

func (c *IrcFsChan) chanDispatch() {
	for msg := range c.incoming {
		log.Println(c.name, msg)
		// TODO dispatch to any active fids
	}
}

func wrRootCtl(line string) error {
	cmd := strings.Fields(line)
	switch (cmd[0]) {
	case "connect":
		if len(cmd) < 2 {
			return errors.New("invalid argument")
		}
		if root.irc != nil {
			return errors.New("already connected")
		}
		conn, err := net.Dial("tcp", cmd[1])
		if err != nil {
			return err
		}
		root.content = make(chan irc.Content)
		go root.dispatch()
		root.irc = irc.InitConn(conn, root.content, nil, nil)
		/* TODO on disconnect, close(root.content) and set root.irc = nil */
		return nil
	case "join":
		if len(cmd) < 2 {
			return errors.New("invalid argument")
		}
		root.irc.Join(cmd[1])
		return nil
	}
	return errors.New("invalid argument")
}

func rdRootEvent() *ReadAux {
	return NewReadAux("", nil)
}

func rdRootNick() *ReadAux {
	c := make(chan string)
	close(c)
	return NewReadAux("sqweek", c)
}

func rdRootPong() *ReadAux {
	return NewReadAux("", nil)
}

func wrChanCtlFn(channel string) WriteLineFn {
	return func(line string) error {
		return nil
	}
}

func wrChanDataFn(channel string) WriteLineFn {
	return func(line string) error {
		return nil
	}
}

func rdChanDataFn(channel string) ReadInitFn {
	return func() *ReadAux {
		return NewReadAux("", nil)
	}
}

func rdChanUsersFn(channel string) ReadInitFn {
	return func() *ReadAux {
		return NewReadAux("", nil)
	}
}

func main() {
	uid = p.OsUsers.Uid2User(os.Geteuid())
	gid = p.OsUsers.Gid2Group(os.Getegid())
	log.Println(uid, gid)
	root.dir = new(srv.File)
	root.content = make(chan irc.Content)
	root.chans = make(map[string] *IrcFsChan)
	err := root.dir.Add(nil, "/", uid, gid, p.DMDIR|0777, nil)
	if err != nil {
		panic(err)
	}
	root.ctl = newLineFile("ctl", 0222, nil, wrRootCtl)
	root.event = newLineFile("event", 0444, rdRootEvent, nil)
	root.nick = newLineFile("nick", 0444, rdRootNick, nil)
	root.pong = newLineFile("pong", 0444, rdRootPong, nil)
	err = add(root.dir, root.ctl, root.event, root.nick, root.pong)
	if err != nil {
		panic(err)
	}

	s := srv.NewFileSrv(root.dir)
	s.Dotu = false
	s.Debuglevel = srv.DbgPrintFcalls
	if !s.Start(s) {
		log.Fatal("Fsrv.Start failed")
	}
	listener, err := net.Listen("unix", "/tmp/ns.sqweek.:0/irc")
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	sigchan := make(chan os.Signal)
	go func() {
		<-sigchan
		listener.Close()
		os.Exit(1)
	}()
	signal.Notify(sigchan, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)

	err = s.StartListener(listener)
	if err != nil {
		log.Fatal(err)
	}
}