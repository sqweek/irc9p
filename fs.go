package main

import (
	"github.com/sqweek/irc9p/irc"
	"github.com/sqweek/p9p-util/p9p"
	"code.google.com/p/go9p/p"
	"code.google.com/p/go9p/p/srv"
	"crypto/tls"
	"strconv"
	"strings"
	"bytes"
	"errors"
	"time"
	"flag"
	"fmt"
	"log"
	"net"
	"sort"
	"os"
	"io"
)

var uid p.User
var gid p.Group

var Einval = errors.New("invalid argument")
var Econnected = errors.New("already connected")
var Edisconnected = errors.New("not connected")

var root IrcFsRoot

type IrcLogger struct {
	dir string
	fd map[string] io.WriteCloser
}

func (logger *IrcLogger) Log(channel, content string) {
	var err error
	tstamp := time.Now().In(time.UTC).Format("2006-01-02 15:04:05")
	for _, aux := range root.data.rdaux {
		aux.Send(fmt.Sprintf("%s  %s  %s", tstamp, channel, content))
	}
	if len(logger.dir) == 0 {
		return
	}
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
	irc *irc.Conn // managed by dialler func
	Connect chan chan error
	Disconnect chan chan bool

	messages chan irc.Event
	chans map[string] *IrcFsChan

	logger IrcLogger

	state struct {
		host string	
		port int
		ssl bool
		nick string /* nick requested by user */
		conn bool /* true if a connection is active or pending */
		password string
	}

	dir *srv.File
	ctl *LineFile
	evfile *LineFile
	data *LineFile
	nick *LineFile
	pong *LineFile
}

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
	return i != len(c.users) && c.users[i] == nick
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
	timedout := false
	for event := range root.messages {
		switch event := event.(type) {
		case *irc.LostContactEvent:
			timedout = true
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
	req := make(chan bool)
	root.Disconnect <-req
	<-req
	root.event("disconnected")
	if timedout {
		go func() {
			for {
				req := make(chan error)
				root.Connect <-req
				err := <-req
				if err == nil {
					return
				}
				time.Sleep(30*time.Second)
			}
		}()
	}
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
		root.event("join %s", fsName(name))
		go c.chanDispatch()
		root.chans[key] = c
	}
	return c
}

func (root *IrcFsRoot) rmChannel(name string) *IrcFsChan {
	key := strings.ToUpper(name)
	c, ok := root.chans[key]
	if ok {
		c.dir.Remove()
		delete(root.chans, key)
		root.event("part %s", fsName(c.name))
	}
	return c
}

func (c *IrcFsChan) chanDispatch() {
	for msg := range c.incoming {
		tstamp := time.Now().Format("15:04:05")
		root.logger.Log(c.name, msg)
		log.Println(c.name, msg)
		msg = fmt.Sprintf("%s %s", tstamp, msg)
		for _, aux := range(c.data.rdaux) {
			if aux.lines != nil {
				aux.Send(msg)
			}
		}
	}
	for _, aux := range(c.data.rdaux) {
		if aux.lines != nil {
			close(aux.lines)
		}
	}
}

func (root *IrcFsRoot) event(format string, args ...interface{}) {
	ev := fmt.Sprintf(format, args...)
	for _, aux := range(root.evfile.rdaux) {
		if aux.lines != nil {
			aux.Send(ev)
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
	root.event("dial %s", addr)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if root.state.ssl {
		// TODO check certificate
		conf := tls.Config{InsecureSkipVerify: true}
		return tls.Client(conn, &conf), nil
	}
	return conn, nil
}

func dialler(root *IrcFsRoot) {
	for {
		select {
		case req := <-root.Disconnect:
			root.state.conn = false
			if root.irc != nil {
				root.irc.Disconnect()
				root.irc = nil
				req <- true
			} else {
				req <- false
			}
		case req := <-root.Connect:
			root.state.conn = true
			if root.irc != nil {
				req <- nil
				continue
			}
			conn, err := root.dial()
			if err != nil {
				root.state.conn = false
				req <- err
			} else {
				root.messages = make(chan irc.Event)
				go root.dispatch()
				iconn := irc.InitConn(conn, root.state.nick, root.state.password, root.messages)
				root.irc = iconn
				root.event("connected")
				for _, ircChan := range root.chans {
					iconn.Join(ircChan.name)
				}
				req <- nil
			}
		}
	}
}

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
			req := make(chan error)
			root.Connect <-req
			err := <-req
			if err != nil {
				return err
			}
			return nil
		case "disconnect":
			req := make(chan bool)
			root.Disconnect <-req
			if !<-req {
				return Edisconnected
			}
			return nil
		}
	case 2:
		/* commands with one argument */
		switch cmd[0] {
		case "join":
			if !root.InChannel(cmd[1]) {
				root.channel(cmd[1]) /* creates dir */
			}
			irc := root.irc
			if irc != nil {
				irc.Join(cmd[1]) // ignore any disconnected error
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
		case "pass":
			root.state.password = cmd[1]
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
	buf += ctlStateString(true, "conn %s", dialState())
	buf += ctlStateString(len(root.state.host) != 0, "server %s", root.state.host)
	buf += ctlStateString(root.state.port != 0, "port %d", root.state.port)
	buf += ctlStateString(root.state.ssl, "ssl %t", root.state.ssl)
	buf += ctlStateString(len(root.logger.dir) != 0, "logdir %s", root.logger.dir)
	buf += ctlStateString(len(root.state.nick) != 0, "nick %s", root.state.nick)
	if len(root.state.password) != 0 {
		buf += "#pass ********"
	}
	return NewReadAux(buf, false)
}

func dialState() string {
	if root.irc != nil {
		return "connected"
	} else if root.state.conn {
		return "dialling"
	}
	return "disconnected"
}

func rdRootEvent() *ReadAux {
	buf := fmt.Sprintf("%s\n", dialState())
	for _, c := range root.chans {
		buf += "join " + fsName(c.name) + "\n"
	}
	return NewReadAux(buf, true)
}

func rdRootLog() *ReadAux {
	return NewReadAux("", true)
}

func rdRootNick() *ReadAux {
	/* FIXME should reflect actual nick */
	return NewReadAux(root.state.nick, false)
}

func rdRootPong() *ReadAux {
	return NewReadAux("", false)
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
					// ignore error from Part; just means we're disconnected
					irc.Part(channel)
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
		err := irc.PrivMsg(channel, line)
		if err != nil {
			return err
		}
		return nil
	}
}

func rdChanDataFn(channel string) ReadInitFn {
	return func() *ReadAux {
		return NewReadAux("", true)
	}
}

func rdChanUsersFn(channel string) ReadInitFn {
	return func() *ReadAux {
		return NewReadAux(strings.Join(root.channel(channel).users, "\n") + "\n", false)
	}
}

var addr = flag.String("addr", "irc", "service name or dial string to announce as")

func main() {
	flag.StringVar(&root.state.host, "host", "", "irc server host")
	flag.IntVar(&root.state.port, "port", 0, "irc server port")
	flag.BoolVar(&root.state.ssl, "ssl", false, "use ssl")
	flag.StringVar(&root.state.nick, "nick", "", "irc nickname")
	flag.StringVar(&root.logger.dir, "logdir", "", "log channel activity here")
	flag.Parse()
	uid = p.OsUsers.Uid2User(os.Geteuid())
	gid = p.OsUsers.Gid2Group(os.Getegid())
	log.Println(uid, gid)
	root.dir = new(srv.File)
	root.messages = make(chan irc.Event)
	root.Connect = make(chan chan error)
	root.Disconnect = make(chan chan bool)
	root.chans = make(map[string] *IrcFsChan)
	root.ctl = newLineFile("ctl", 0666, rdRootCtl, wrRootCtl)
	root.evfile = newLineFile("event", 0444, rdRootEvent, nil)
	root.data = newLineFile("data", 0444, rdRootLog, nil)
	root.nick = newLineFile("nick", 0444, rdRootNick, nil)
	root.pong = newLineFile("pong", 0444, rdRootPong, nil)
	root.logger.fd = make(map[string] io.WriteCloser)
	err := root.dir.Add(nil, "/", uid, gid, p.DMDIR|0777, nil)
	if err != nil {
		panic(err)
	}
	err = add(root.dir, root.ctl, root.evfile, root.data, root.nick, root.pong)
	if err != nil {
		panic(err)
	}
	go dialler(&root)

	s := srv.NewFileSrv(root.dir)
	s.Dotu = false
	//s.Debuglevel = srv.DbgPrintFcalls
	if !s.Start(s) {
		log.Fatal("Fsrv.Start failed")
	}
	listener, err := p9p.ListenSrv(*addr)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	p9p.CloseOnSignal(listener)

	err = s.StartListener(listener)
	if err != nil {
		log.Fatal(err)
	}
}