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
	nick string // the actual nick; may be different from requested nick

	broadcast chan Event
	listeners map[chan Event] bool
}

type Event interface {
	/* each event is associated with a "clique". this is one of:
	 * a) an empty string (for server-related events)
	 * b) name of the channel this event refers to (begins with '#' '+' or '&')
	 * c) name of the user that initiated this event */
	Clique() string
	String() string
}

type Clq struct {
	clique string
}
func (e *Clq) Clique() string { return e.clique }

type ServerEvent struct {
	src string
	cmd string
	text string
	ResponseCode int // -1 for non-numeric message
}
func (e *ServerEvent) Clique() string { return "" }
func (e *ServerEvent) String() string { return strings.TrimLeft(fmt.Sprintf("%s %s %s", e.src, e.cmd, e.text), " ") }

type ServerMsgEvent struct {
	ServerEvent
}

type PrivMsgEvent struct {
	Clq
	src string
	msg string
}
func (e *PrivMsgEvent) String() string { return fmt.Sprintf("<%s> %s", e.src, e.msg) }

type NoticeEvent struct {
	PrivMsgEvent
}

type JoinEvent struct {
	Clq
	Nick string
	userinfo string
}
func (e *JoinEvent) String() string { return fmt.Sprintf("-> %s (%s) has joined %s", e.Nick, e.userinfo, e.clique) }

type PartEvent struct {
	Clq
	Nick string
	userinfo string
}
func (e *PartEvent) String() string { return fmt.Sprintf("<- %s (%s) has left %s", e.Nick, e.userinfo, e.clique) }

type QuitEvent struct {
	Clq // nick
	userinfo string
	msg string
}
func (e *QuitEvent) String() string { return fmt.Sprintf("<- %s (%s) has quit IRC: %s", e.clique, e.userinfo, e.msg) }
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

func LooksLikeChannel(dest string) bool {
	return len(dest) > 0 && strings.ContainsAny(dest[0:1], "#&+")
}

func LooksLikeServer(src string) bool {
	return len(src) == 0 || strings.Contains(src, ".")
}

func InitConn(conn io.ReadWriter, listener chan Event, nick, pass *string) *Conn {
	irc := Conn{make(chan string), conn, "", make(chan Event), make(map[chan Event] bool)}
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
	if len(strings.TrimLeft(cmd.cmd, "0123456789")) == 0 {
		code := -1
		fmt.Sscan(cmd.cmd, &code)
		return &ServerEvent{cmd.prefix.nick, cmd.cmd, strings.Join(cmd.params, " "), code}, nil
	}
	command := strings.ToUpper(cmd.cmd)
	switch (command) {
	case "PRIVMSG", "NOTICE":
		if len(cmd.params) < 2 {
			return nil, Eparams
		}
		dest := cmd.params[0]
		msg := cmd.params[1]
		ev := PrivMsgEvent{Clq{dest}, cmd.prefix.nick, msg}
		if !LooksLikeChannel(dest) {
			if LooksLikeServer(cmd.prefix.nick) {
				return &ServerMsgEvent{ServerEvent{cmd.prefix.nick, command, ev.msg, -1}}, nil
			}
			ev.clique = cmd.prefix.nick
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
		return &JoinEvent{Clq{cmd.params[0]}, cmd.prefix.nick, cmd.prefix.Join()}, nil
	case "PART":
		if len(cmd.params) < 1 {
			return nil, Eparams
		}
		return &PartEvent{Clq{cmd.params[0]}, cmd.prefix.nick, cmd.prefix.Join()}, nil
	case "QUIT":
		return &QuitEvent{Clq{cmd.prefix.nick}, cmd.prefix.Join(), strings.Join(cmd.params, " ")}, nil
	}
	return nil, Eunsupp
}

func (irc *Conn) sender() {
	for line := range irc.send {
		irc.conn.Write([]byte(line))
	}
}
