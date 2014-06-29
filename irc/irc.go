package irc

import (
	"regexp"
	"strings"
	"errors"
	"bufio"
	"fmt"
	"log"
	"io"
)

type Conn struct {
	send chan string
	conn io.ReadWriter

	broadcast chan Event
	listeners map[chan Event] bool
}

type Event interface {
	From() string
	To() string
	String() string
}

type Src struct {
	src string
}
func (e *Src) From() string { return e.src }

type Dest struct {
	dest string
}
func (e *Dest) To() string { return e.dest }

type ServerEvent struct {
	Src
	cmd string
	text string
}
func (e *ServerEvent) To() string { return "" }
func (e *ServerEvent) String() string { return strings.TrimLeft(fmt.Sprintf("%s %s %s", e.src, e.cmd, e.text), " ") }

type PrivMsgEvent struct {
	Src // user (never server/channel)
	Dest // channel or self
	msg string
}
func (e *PrivMsgEvent) String() string { return fmt.Sprintf("<%s> %s", e.src, e.msg) }

type NoticeEvent struct {
	PrivMsgEvent
}

type JoinEvent struct {
	Src // user
	Dest // channel
	userinfo string
}
func (e *JoinEvent) String() string { return fmt.Sprintf("-> %s (%s) has joined %s", e.src, e.userinfo, e.dest) }

type PartEvent struct {
	Src // user
	Dest // channel
	userinfo string
}
func (e *PartEvent) String() string { return fmt.Sprintf("<- %s (%s) has left %s", e.src, e.userinfo, e.dest) }

var reCmd *regexp.Regexp = regexp.MustCompile("[0-9][0-9][0-9]|[a-zA-Z]+")

var Eunsupp = errors.New("unhandled command")
var Eparams = errors.New("insufficient parameters")

type ircPrefix struct {
	nick string
	user string
	host string
}

type ircCmd struct {
	prefix ircPrefix
	cmd string
	params []string
}

func isToChannel(e Event) bool {
	return len(e.To()) > 0 && strings.ContainsAny(e.To()[0:1], "#&+")
}

func isFromServer(e Event) bool {
	return len(e.From()) == 0 || strings.Contains(e.From(), ".")
}

func InitConn(conn io.ReadWriter, listener chan Event, nick, pass *string) *Conn {
	irc := Conn{make(chan string), conn, make(chan Event), make(map[chan Event] bool)}
	irc.Listen(listener)
	lines := make(chan string)
	go readlines(irc.conn, lines)
	go irc.parser(lines)
	go irc.broadcaster()
	go irc.sender()
	if pass != nil {
		irc.send <- fmt.Sprintf("PASS %s\r\n", *pass)
	}
	var n string
	if nick == nil {
		n = "irc9p-guest"
	} else {
		n = *nick
	}
	irc.send <- fmt.Sprintf("NICK %s\r\n", n)
	irc.send <- fmt.Sprintf("USER %s 0 0 :%s\r\n", n, n)
	return &irc
}

func (irc *Conn) PrivMsg(channel, msg string) {
	irc.send <- fmt.Sprintf("PRIVMSG %s :%s\r\n", channel, msg)
}

func (irc *Conn) Join(channel string) {
	irc.send <- fmt.Sprintf("JOIN %s\r\n", channel)
}

func (irc *Conn) Listen(listener chan Event) {
	irc.listeners[listener] = true
}

func (irc *Conn) broadcaster() {
	for content := range irc.broadcast {
		for listener, ok := range irc.listeners {
			if !ok {
				continue
			}
			listener <- content
		}
	}
}

func readlines(input io.Reader, lines chan string) {
	defer close(lines)
	rd := bufio.NewReader(input)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					lines <- line
				}
				return
			}
			log.Println("read error: ", err)
			return
		}
		lines <- line
	}
}

func gettok(s, delim string) (token string, rest string) {
	f := strings.SplitN(s, delim, 2)
	if len(f) < 2 {
		return f[0], ""
	}
	return f[0], strings.TrimLeft(f[1], delim)
}

func parseCmd(line string) (*ircCmd, error) {
	var cmd ircCmd
	line = strings.TrimRight(line, "\r\n")
	prefix, rest := "", line
	if len(line) >0 && line[0] == ':' {
		prefix, rest = gettok(line, " ")
		nick, prest := gettok(prefix[1:], "!")
		user, host := gettok(prest, "@")
		cmd.prefix.nick = nick
		cmd.prefix.user = user
		cmd.prefix.host = host
	}
	if len(rest) == 0 {
		return nil, nil
	}
	name, params := gettok(rest, " ")
	if !reCmd.MatchString(name) || len(params) == 0 {
		return nil, errors.New("malformed command")
	}
	cmd.cmd = name
	for len(params) > 0 {
		if params[0] == ':' {
			cmd.params = append(cmd.params, params[1:])
			break
		}
		var param string
		param, params = gettok(params, " ")
		cmd.params = append(cmd.params, param)
	}
	return &cmd, nil
}

func (irc *Conn) parser(lines chan string) {
	defer close(irc.broadcast)
	for line := range lines {
		cmd, err := parseCmd(line)
		if err != nil {
			log.Printf("%v: %s\n", err, line)
			continue
		}
		if cmd != nil {
			event, err := irc.newEvent(cmd)
			if err != nil {
				log.Printf("%v: %s\n", err, line)
			}
			if event != nil {
				irc.broadcast <- event
			}
		}
	}
}

func (pre ircPrefix) Join() string {
	s := pre.nick
	if len(pre.user) > 0 {
		s = s + "!" + pre.user
	}
	if len(pre.host) > 0 {
		s = s + "@" + pre.host
	}
	return s
}

func (irc *Conn) newEvent(cmd *ircCmd) (Event, error) {
	command := strings.ToUpper(cmd.cmd)
	switch (command) {
	case "PRIVMSG", "NOTICE":
		if len(cmd.params) < 2 {
			return nil, Eparams
		}
		ev := PrivMsgEvent{Src{cmd.prefix.nick}, Dest{cmd.params[0]}, cmd.params[1]}
		if !isToChannel(&ev) {
			ev.dest = ""
			if isFromServer(&ev) {
				return &ServerEvent{Src{ev.src}, command, ev.msg}, nil
			}
		}
		if command == "PRIVMSG" {
			return &ev, nil
		}
		return &NoticeEvent{ev}, nil
	case "PING":
		irc.send <- fmt.Sprintf("PONG :\r\n")
		return nil, nil
	case "JOIN":
		if len(cmd.params) < 1 {
			return nil, Eparams
		}
		return &JoinEvent{Src{cmd.prefix.nick}, Dest{cmd.params[0]}, cmd.prefix.Join()}, nil
	case "PART":
		if len(cmd.params) < 1 {
			return nil, Eparams
		}
		return &PartEvent{Src{cmd.prefix.nick}, Dest{cmd.params[0]}, cmd.prefix.Join()}, nil
	}
	log.Printf("'%s' '%s'\n", cmd.prefix.Join(), cmd.cmd)
	for _, p := range cmd.params {
		log.Printf("'%s'\n", p)
	}
	return nil, Eunsupp
}

func (irc *Conn) sender() {
	for line := range irc.send {
		irc.conn.Write([]byte(line))
	}
}
