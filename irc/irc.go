package irc

import (
	"regexp"
	"strconv"
	"strings"
	"errors"
	"bufio"
	"sync"
	"fmt"
	"log"
	"io"
)

var Edisconnected = errors.New("not connected")

type Conn struct {
	sendchan chan string
	sendLock sync.Mutex
	conn io.ReadWriteCloser
	nick string // the actual nick; may be different from requested nick

	broadcast chan Event
	listeners map[chan Event] bool

	tmp struct {
		names map[string] []string
	}
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

type NamesEvent struct {
	Clq
	Names []string
}
func (e *NamesEvent) String() string { return "[" + strings.Join(e.Names, " ") + "]" }

type NickEvent struct {
	Clq // original nick
	NewNick string
}
func (e *NickEvent) String() string { return fmt.Sprintf("! %s is now known as %s", e.clique, e.NewNick) }

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

type TopicEvent struct {
	Clq
	Topic string
}
func (e *TopicEvent) String() string { return "== " + e.Topic + " ==" }

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

func (cmd *ircCmd) Param(i int, dflt string) string {
	if i >= 0 && i < len(cmd.params) {
		return cmd.params[i]
	}
	return dflt
}

func LooksLikeChannel(dest string) bool {
	return len(dest) > 0 && strings.ContainsAny(dest[0:1], "#&+")
}

func LooksLikeServer(src string) bool {
	return len(src) == 0 || strings.Contains(src, ".")
}

func InitConn(conn io.ReadWriteCloser, nick string, pass *string, listeners ...chan Event) *Conn {
	irc := Conn{sendchan: make(chan string), conn: conn, broadcast: make(chan Event), listeners: make(map[chan Event] bool)}
	irc.tmp.names = make(map[string] []string)
	for _, listener := range listeners {
		irc.Listen(listener)
	}
	lines := make(chan string)
	go readlines(irc.conn, lines)
	go irc.parser(lines)
	go irc.broadcaster()
	go irc.sender()
	if pass != nil {
		irc.send("PASS %s", *pass)
	}
	if len(nick) == 0 {
		nick = "irc9p-guest"
	}
	irc.send("NICK %s", nick)
	irc.nick = nick
	irc.send("USER %s 0 0 :%s", nick, nick)
	return &irc
}

func (irc *Conn) PrivMsg(channel, msg string) error {
	err := irc.send("PRIVMSG %s :%s\r\n", channel, msg)
	if err != nil {
		return err
	}
	irc.broadcast <- &PrivMsgEvent{Clq{channel}, irc.nick, msg}
	return nil
}

func (irc *Conn) Join(channel string) error {
	if !LooksLikeChannel(channel) {
		return nil
	}
	return irc.send("JOIN %s\r\n", channel)
}

func (irc *Conn) Part(channel string) error {
	if !LooksLikeChannel(channel) {
		return nil
	}
	return irc.send("PART %s\r\n", channel)
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
	for listener, ok := range irc.listeners {
		if ok {
			close(listener)
		}
	}
	irc.sendLock.Lock()
	close(irc.sendchan)
	irc.sendchan = nil
	irc.sendLock.Unlock()
	irc.conn.Close()
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
		code, _ := strconv.Atoi(cmd.cmd)
		n := len(cmd.params)
		switch code {
		case 332:
			channel, topic := cmd.Param(1, ""), cmd.Param(2, "")
			if len(channel) == 0 {
				return nil, Eunsupp
			}
			return &TopicEvent{Clq{channel}, topic}, nil
		case 353:
			if n < 2 {
				return nil, Eunsupp
			}
			channel := cmd.params[n - 2]
			names := irc.tmp.names[channel]
			irc.tmp.names[channel] = append(names, strings.Fields(cmd.params[n - 1])...)
			return nil, nil
		case 366:
			if n < 2 {
				return nil, Eunsupp
			}
			channel := cmd.params[1]
			names := irc.tmp.names[channel]
			irc.tmp.names[channel] = nil
			return &NamesEvent{Clq{channel}, names}, nil
		}
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
		irc.send("PONG :")
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
	case "NICK":
		if len(cmd.params) < 1 {
			return nil, Eparams
		}
		return &NickEvent{Clq{cmd.prefix.nick}, cmd.params[0]}, nil
	case "QUIT":
		return &QuitEvent{Clq{cmd.prefix.nick}, cmd.prefix.Join(), strings.Join(cmd.params, " ")}, nil
	}
	return nil, Eunsupp
}

func (irc *Conn) send(format string, args ...interface{}) error {
	irc.sendLock.Lock()
	defer irc.sendLock.Unlock()
	if irc.send != nil {
		irc.sendchan <- fmt.Sprintf(format + "\r\n", args...)
	} else {
		return Edisconnected
	}
	return nil
}

func (irc *Conn) sender() {
	for line := range irc.sendchan {
		irc.conn.Write([]byte(line))
	}
}
