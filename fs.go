package main

import (
	"github.com/sqweek/irc9p/irc"
	"code.google.com/p/go9p/p"
	"code.google.com/p/go9p/p/srv"
	"crypto/tls"
	"strconv"
	"strings"
	"bytes"
	"errors"
	"time"
	"fmt"
	"log"
	"net"
	"sort"
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
	rdaux, ok := f.rdaux[fid.Fid]
	if ok {
		delete(f.rdaux, fid.Fid)
		if rdaux.lines != nil {
			close(rdaux.lines)
		}
	}
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
	if rdaux.lines == nil {
		return n, nil
	}
	line, ok := <-rdaux.lines
	if !ok {
		return n, nil
	} else if n + len(line) + 1 > len(data) {
		rdaux.buf.WriteString(line)
		rdaux.buf.Write([]byte{'\n'})
		n2, _ := rdaux.buf.Read(data[n:])
		return n + n2, nil
	} else {
		n2 := copy(data[n:], line + "\n")
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
		err = callback(strings.TrimRight(line, "\r\n"))
		if err != nil {
			return err
		}
	}
}

var uid p.User
var gid p.Group

type IrcLogger struct {
	dir string
	fd map[string] io.WriteCloser
}

func (logger *IrcLogger) Log(channel, content string) {
	var err error
	tstamp := time.Now().In(time.UTC).Format("2006-01-02 15:04:05")
	fd, ok := logger.fd[channel]
	if !ok {
		fd, err = os.OpenFile(logger.dir + "/" + channel + ".log", os.O_WRONLY | os.O_APPEND | os.O_CREATE, 0644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open failed: ", err)
			return
		}
		logger.fd[channel] = fd
	}
	fmt.Fprintln(fd, tstamp, content)
}

func (logger *IrcLogger) Close(channel string) {
	fd, ok := logger.fd[channel]
	if ok {
		fd.Close()
		delete(logger.fd, channel)
	}
}

type IrcFsRoot struct {
	irc *irc.Conn
	messages chan irc.Event
	chans map[string] *IrcFsChan

	logger IrcLogger

	state struct {
		host string	
		port int
		ssl bool
		nick string /* nick requested by user */
		conn bool /* true if a connection is active or pending */
	}

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
	users []string

	dir *srv.File
	ctl *LineFile
	data *LineFile
	userfile *LineFile // optional; can be nil
}

func (c *IrcFsChan) HasNick(nick string) bool {
	i := sort.SearchStrings(c.users, nick)
	return i == len(c.users) || c.users[i] == nick
}

func (c *IrcFsChan) Joined(nick string) {
	i := sort.SearchStrings(c.users, nick)
	if i < len(c.users) && c.users[i] == nick {
		return
	}
	c.users = append(c.users, "")
	copy(c.users[i + 1:], c.users[i:])
	c.users[i] = nick
}

func (c *IrcFsChan) Parted(nick string) {
	i := sort.SearchStrings(c.users, nick)
	if i == len(c.users) || c.users[i] != nick {
		return
	}
	c.users = append(c.users[:i], c.users[i + 1:]...)
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
		if child == nil {
			continue
		}
		err := child.Add(parent, child.name, uid, gid, child.mode, child)
		if err != nil {
			return err
		}
	}
	return nil
}

/* Adds a channel directory and ctl/data files under the given parent dir */
func (ch *IrcFsChan) Add(parent *srv.File) error {
	err := ch.dir.Add(parent, fsName(ch.name), uid, gid, p.DMDIR|0777, nil)
	if err != nil {
		return err
	}
	err = add(ch.dir, ch.ctl, ch.data, ch.userfile)
	if err != nil {
		return err
	}
	return nil
}

func newFsChan(channel string) *IrcFsChan {
	var users *LineFile = nil
	if irc.LooksLikeChannel(channel) {
		users = newLineFile("users", 0444, rdChanUsersFn(channel), nil)
	}
	return &IrcFsChan{
		make(chan string),
		channel,
		make([]string, 0, 32),
		new(srv.File),
		newLineFile("ctl", 0222, nil, wrChanCtlFn(channel)),
		newLineFile("data", 0666, rdChanDataFn(channel), wrChanDataFn(channel)),
		users,
	}
}

func (root *IrcFsRoot) dispatch() {
	for event := range root.messages {
		switch event := event.(type) {
		case *irc.ServerEvent, *irc.ServerMsgEvent:
			log.Println("server msg:", event)
		case *irc.NamesEvent:
			ircChan := root.channel(event.Clique())
			ircChan.users = event.Names
			sort.Strings(ircChan.users)
			ircChan.incoming <- event.String()
		case *irc.QuitEvent:
			for _, ircChan := range root.chans {
				if ircChan.HasNick(event.Clique()) {
					ircChan.Parted(event.Clique())
					ircChan.incoming <- event.String()
				}
			}
		case *irc.NickEvent:
			for _, ircChan := range root.chans {
				if ircChan.HasNick(event.Clique()) {
					ircChan.Parted(event.Clique())
					ircChan.Joined(event.NewNick)
					ircChan.incoming <- event.String()
				}
			}
		case *irc.PartEvent:
			ircChan := root.channel(event.Clique())
			ircChan.Parted(event.Nick)
			ircChan.incoming <- event.String()
		case *irc.JoinEvent:
			ircChan := root.channel(event.Clique())
			ircChan.Joined(event.Nick)
			ircChan.incoming <- event.String()
		default:
			channame := event.Clique()
			root.channel(channame).incoming <- event.String()
		}
	}
	root.state.conn = false
	root.irc = nil
}

func fsName(ircName string) string {
	return strings.Replace(ircName, "/", "␣", -1)
}

func (root *IrcFsRoot) InChannel(name string) bool {
	key := strings.ToUpper(name)
	_, ok := root.chans[key]
	return ok
}

func (root *IrcFsRoot) channel(name string) *IrcFsChan {
	key := strings.ToUpper(name)
	c, ok := root.chans[key]
	if !ok {
		c = newFsChan(name)
		c.Add(root.dir)
		go c.chanDispatch()
		root.chans[key] = c
	}
	return c
}

func (root *IrcFsRoot) rmChannel(name string) *IrcFsChan {
	key := strings.ToUpper(name)
	c, ok := root.chans[key]
	if ok {
		delete(root.chans, key)
	}
	return c
}

func (c *IrcFsChan) chanDispatch() {
	for msg := range c.incoming {
		tstamp := time.Now().Format("15:04:05")
		if len(root.logger.dir) > 0 {
			root.logger.Log(c.name, msg)
		}
		log.Println(c.name, msg)
		msg = fmt.Sprintf("%s %s", tstamp, msg)
		for _, aux := range(c.data.rdaux) {
			if aux.lines != nil {
				aux.lines <- msg
			}
		}
	}
	for _, aux := range(c.data.rdaux) {
		if aux.lines != nil {
			close(aux.lines)
		}
	}
}

func (root *IrcFsRoot) dial() (net.Conn, error) {
	if len(root.state.host) == 0 {
		return nil, errors.New("no server specified")
	}
	port := root.state.port
	if port == 0 {
		if root.state.ssl {
			port = 6697
		} else {
			port = 6667
		}
	}
	addr := fmt.Sprintf("%s:%d", root.state.host, port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if root.state.ssl {
		conf := tls.Config{InsecureSkipVerify: true}
		return tls.Client(conn, &conf), nil
	}
	return conn, nil
}

var Einval = errors.New("invalid argument")
var Econnected = errors.New("already connected")
var Edisconnected = errors.New("not connected")

func wrRootCtl(line string) error {
	if len(line) == 0 || line[0] == '#' {
		return nil
	}
	cmd := strings.Fields(line)
	switch len(cmd) {
	case 0:
		return nil /* ignore empty lines */
	case 1:
		/* commands with no arguments */
		switch cmd[0] {
		case "connect":
			if root.irc != nil || root.state.conn {
				return Econnected
			}
			root.state.conn = true
			conn, err := root.dial()
			if err != nil {
				root.state.conn = false
				return err
			}
			root.messages = make(chan irc.Event)
			go root.dispatch()
			root.irc = irc.InitConn(conn, root.state.nick, nil, root.messages)
			/* TODO rejoin any open channels */
			return nil
		}
	case 2:
		/* commands with one argument */
		switch cmd[0] {
		case "join":
			irc := root.irc
			if irc == nil {
				return Edisconnected
			}
			if !root.InChannel(cmd[1]) {
				root.channel(cmd[1]) /* creates dir */
				irc.Join(cmd[1])
			}
			return nil
		case "server":
			root.state.host = cmd[1]
			return nil
		case "port":
			if len(strings.TrimLeft(cmd[1], "0123456789")) > 0 {
				return Einval
			}
			root.state.port, _ = strconv.Atoi(cmd[1])
			return nil
		case "ssl":
			switch cmd[1] {
			case "on", "yes", "true":
				root.state.ssl = true
			case "off", "no", "false":
				root.state.ssl = false
			default:
				return Einval
			}
			return nil
		case "nick":
			root.state.nick = cmd[1]
			return nil
		case "logdir":
			/* confirm directory exists */
			_, err := os.Stat(cmd[1])
			if err != nil {
				return err
			}
			root.logger.dir = cmd[1]
			return nil
		}
	}
	return Einval
}

func ctlStateString(cond bool, format string, args ...interface{}) string {
	if !cond {
		format = "#" + format
	}
	return fmt.Sprintf(format + "\n", args...)
}

func rdRootCtl() *ReadAux {
	var buf string
	buf += ctlStateString(len(root.state.host) != 0, "server %s", root.state.host)
	buf += ctlStateString(root.state.port != 0, "port %d", root.state.port)
	buf += ctlStateString(root.state.ssl, "ssl %t", root.state.ssl)
	buf += ctlStateString(len(root.logger.dir) != 0, "logdir %s", root.logger.dir)
	buf += ctlStateString(len(root.state.nick) != 0, "nick %s", root.state.nick)
	return NewReadAux(buf, nil)
}

func rdRootEvent() *ReadAux { /* TODO */
	return NewReadAux("", nil)
}

func rdRootNick() *ReadAux {
	/* FIXME should reflect actual nick */
	return NewReadAux(root.state.nick, nil)
}

func rdRootPong() *ReadAux {
	return NewReadAux("", nil)
}

func wrChanCtlFn(channel string) WriteLineFn {
	return func(line string) error {
		if len(line) == 0 || line[0] == '#' {
			return nil
		}
		cmd := strings.Fields(line)
		switch len(cmd) {
		case 0:
			return nil /* ignore empty lines */
		case 1:
			switch cmd[0] {
			case "part":
				ircChan := root.rmChannel(channel)
				irc := root.irc
				if irc != nil {
					root.irc.Part(channel)
				}
				close(ircChan.incoming)
				return nil
			}
		}
		return Einval
	}
}

func wrChanDataFn(channel string) WriteLineFn {
	return func(line string) error {
		irc := root.irc
		if irc == nil {
			return Edisconnected
		}
		irc.PrivMsg(channel, line)
		return nil
	}
}

func rdChanDataFn(channel string) ReadInitFn {
	return func() *ReadAux {
		return NewReadAux("", make(chan string))
	}
}

func rdChanUsersFn(channel string) ReadInitFn {
	return func() *ReadAux {
		return NewReadAux(strings.Join(root.channel(channel).users, "\n") + "\n", nil)
	}
}

func main() {
	uid = p.OsUsers.Uid2User(os.Geteuid())
	gid = p.OsUsers.Gid2Group(os.Getegid())
	log.Println(uid, gid)
	root.dir = new(srv.File)
	root.messages = make(chan irc.Event)
	root.chans = make(map[string] *IrcFsChan)
	root.ctl = newLineFile("ctl", 0666, rdRootCtl, wrRootCtl)
	root.event = newLineFile("event", 0444, rdRootEvent, nil)
	root.nick = newLineFile("nick", 0444, rdRootNick, nil)
	root.pong = newLineFile("pong", 0444, rdRootPong, nil)
	root.logger.fd = make(map[string] io.WriteCloser)
	err := root.dir.Add(nil, "/", uid, gid, p.DMDIR|0777, nil)
	if err != nil {
		panic(err)
	}
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