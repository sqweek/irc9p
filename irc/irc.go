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

	broadcast chan Content
	listeners map[chan Content] bool
	//chans map[string] chan string
}

type Content struct {
	Channel string
	Data string
}

var reCmd *regexp.Regexp = regexp.MustCompile("[0-9][0-9][0-9]|[a-zA-Z]+")

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

func InitConn(conn io.ReadWriter, listener chan Content, nick, pass *string) *Conn {
	irc := Conn{make(chan string), conn, make(chan Content), make(map[chan Content] bool)}
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

func (irc *Conn) Listen(listener chan Content) {
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
			err = irc.recv(cmd)
			if err != nil {
				log.Printf("%v: %s\n", err, line)
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

func (irc *Conn) recv(cmd *ircCmd) error {
	switch (cmd.cmd) {
	case "PRIVMSG", "NOTICE":
		if len(cmd.params) < 2 {
			return errors.New("not enough params")
		}
		target := cmd.params[0]
		text := cmd.params[1]
		switch target[0] {
		case '#', '&', '+':
			/* message to channel */
			line := fmt.Sprintf("<%s> %s", cmd.prefix.nick, text)
			irc.broadcast <- Content{target, line}
			return nil
		default:
			/* message to us personally, or broadcast */
			from := cmd.prefix.nick
			var line string
			if len(from) == 0 || strings.Contains(from, ".") {
				/* no nick, message must be from the server */
				from = ""
				line = text
			} else {
				line = fmt.Sprintf("<%s> %s", from, text)
			}
			irc.broadcast <- Content{from, line}
			return nil
		}
	case "PING":
		irc.send <- fmt.Sprintf("PONG :\r\n")
		
	}
	log.Printf("'%s' '%s'\n", cmd.prefix.Join(), cmd.cmd)
	for _, p := range cmd.params {
		log.Printf("'%s'\n", p)
	}
	return errors.New("unhandled command")
}

func (irc *Conn) sender() {
	for line := range irc.send {
		irc.conn.Write([]byte(line))
	}
}
