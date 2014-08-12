/* Implements line-based 9p operations */
package main

import (
	"bytes"
	"code.google.com/p/go9p/p"
	"code.google.com/p/go9p/p/srv"
	"fmt"
	"io"
	"strings"
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

type WriteLineFn func(string) error

type ReadInitFn func() *ReadAux

type ReadAux struct {
	buf *bytes.Buffer
	lines chan string
	missed int
}

func (aux *ReadAux) Send(line string) bool {
	select {
	case aux.lines <- line:
		return true
	default:
		aux.missed++
	}
	return false
}

func NewReadAux(data string, useChan bool) *ReadAux {
	aux := ReadAux{buf: bytes.NewBufferString(data)}
	if useChan {
		aux.lines = make(chan string, 10)
	}
	return &aux
}

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
	f.Clunk(fid)
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
		delete(f.wraux, fid.Fid)
		err := dowrite(buf, f.wrline, true)
		if err != nil {
			return err
		}
	}
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
		return n, nil
	}
	if rdaux.lines == nil {
		return n, nil
	}
	line, ok := <-rdaux.lines
	if !ok {
		return n, nil
	}
	if rdaux.missed != 0 {
		line = line + fmt.Sprintf("\n<%d lines suppressed>", rdaux.missed)
		rdaux.missed = 0
	}
	if n + len(line) + 1 > len(data) {
		rdaux.buf.WriteString(line)
		rdaux.buf.Write([]byte{'\n'})
		n2, _ := rdaux.buf.Read(data[n:])
		return n + n2, nil
	}
	n2 := copy(data[n:], line + "\n")
	return n + n2, nil
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
	return nil
}

