// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/CodisLabs/codis/pkg/proxy/redis"
	"github.com/CodisLabs/codis/pkg/utils"
	"github.com/CodisLabs/codis/pkg/utils/errors"
	"github.com/CodisLabs/codis/pkg/utils/log"
	"github.com/CodisLabs/codis/pkg/utils/sync2/atomic2"
)

type Session struct {
	Conn *redis.Conn

	Ops int64

	CreateUnix int64
	LastOpUnix int64

	auth string
	quit bool
	exit sync.Once

	stats struct {
		opmap map[string]*opStats
		total atomic2.Int64
		flush uint
	}
	start sync.Once

	authorized bool

	alloc RequestAlloc
}

func (s *Session) String() string {
	o := &struct {
		Ops        int64  `json:"ops"`
		CreateUnix int64  `json:"create"`
		LastOpUnix int64  `json:"lastop,omitempty"`
		RemoteAddr string `json:"remote"`
	}{
		s.Ops, s.CreateUnix, s.LastOpUnix,
		s.Conn.RemoteAddr(),
	}
	b, _ := json.Marshal(o)
	return string(b)
}

func NewSession(conn *redis.Conn, auth string) *Session {
	s := &Session{
		Conn: conn, auth: auth,
		CreateUnix: time.Now().Unix(),
	}
	s.stats.opmap = make(map[string]*opStats, 16)
	log.Infof("session [%p] create: %s", s, s)
	return s
}

func (s *Session) CloseWithError(err error, half bool) {
	s.exit.Do(func() {
		if err != nil {
			log.Infof("session [%p] closed: %s, error: %s", s, s, err)
		} else {
			log.Infof("session [%p] closed: %s, quit", s, s)
		}
	})
	if half {
		s.Conn.CloseReader()
	} else {
		s.Conn.Close()
	}
}

var (
	ErrTooManySessions = errors.New("too many sessions")
	ErrRouterNotOnline = errors.New("router is not online")
)

var RespOK = redis.NewString([]byte("OK"))

func (s *Session) Start(d *Router, config *Config) {
	s.start.Do(func() {
		if int(incrSessions()) > config.ProxyMaxClients {
			go func() {
				s.Conn.Encode(redis.NewErrorf("ERR max number of clients reached"), true)
				s.CloseWithError(ErrTooManySessions, false)
			}()
			decrSessions()
			return
		}

		if !d.isOnline() {
			go func() {
				s.Conn.Encode(redis.NewErrorf("ERR router is not online"), true)
				s.CloseWithError(ErrRouterNotOnline, false)
			}()
			decrSessions()
			return
		}

		tasks := make(chan *Request, config.SessionMaxPipeline)
		var ch = make(chan struct{})

		go func() {
			defer close(ch)
			s.loopWriter(tasks)
		}()

		go func() {
			s.loopReader(tasks, d)
			<-ch
			decrSessions()
		}()
	})
}

func (s *Session) loopReader(tasks chan<- *Request, d *Router) (err error) {
	defer func() {
		if err != nil {
			s.CloseWithError(err, true)
		}
		close(tasks)
	}()
	for !s.quit {
		multi, err := s.Conn.DecodeMultiBulk()
		if err != nil {
			return err
		}
		s.incrOpTotal()

		usnow := utils.Microseconds()
		s.LastOpUnix = usnow / 1e6
		s.Ops++

		r := s.alloc.NewRequest()
		r.Multi = multi
		r.Start = usnow
		r.Batch = s.alloc.NewBatch()
		if err := s.handleRequest(r, d); err != nil {
			r.Resp = redis.NewErrorf("ERR dispatch failed, %s", err)
			tasks <- r
			return s.incrOpFails(err)
		} else {
			tasks <- r
		}
	}
	return nil
}

func (s *Session) loopWriter(tasks <-chan *Request) (err error) {
	defer func() {
		s.CloseWithError(err, false)
		for _ = range tasks {
			s.incrOpFails(nil)
		}
		s.flushOpStats()
	}()

	p := s.Conn.FlushPolicy(128, time.Millisecond)

	for r := range tasks {
		resp, err := s.handleResponse(r)
		if err != nil {
			resp = redis.NewErrorf("ERR backend failure, %s", err)
			p.Conn.Encode(resp, true)
			return s.incrOpFails(err)
		}
		if err := p.Encode(resp); err != nil {
			return s.incrOpFails(err)
		}
		if err := p.Flush(len(tasks) == 0); err != nil {
			return s.incrOpFails(err)
		}
		if len(tasks) == 0 {
			s.flushOpStats()
		}
	}
	return nil
}

func (s *Session) handleResponse(r *Request) (*redis.Resp, error) {
	r.Batch.Wait()
	if r.Coalesce != nil {
		if err := r.Coalesce(); err != nil {
			return nil, err
		}
	}
	if err := r.Err; err != nil {
		return nil, err
	}
	switch resp := r.Resp; {
	case resp == nil:
		return nil, ErrRespIsRequired
	default:
		s.incrOpStats(r)
		return resp, nil
	}
}

func (s *Session) handleRequest(r *Request, d *Router) error {
	opstr, flag, err := getOpInfo(r.Multi)
	if err != nil {
		return err
	}
	r.OpStr = opstr
	r.Dirty = !flag.IsReadOnly()

	if flag.IsNotAllow() {
		return fmt.Errorf("command '%s' is not allowed", opstr)
	}

	switch opstr {
	case "QUIT":
		return s.handleQuit(r)
	case "AUTH":
		return s.handleAuth(r)
	}

	if !s.authorized {
		if s.auth != "" {
			r.Resp = redis.NewErrorf("NOAUTH Authentication required")
			return nil
		}
		s.authorized = true
	}

	switch opstr {
	case "SELECT":
		return s.handleSelect(r)
	case "PING":
		return s.handlePing(r)
	case "MGET":
		return s.handleRequestMGet(r, d)
	case "MSET":
		return s.handleRequestMSet(r, d)
	case "DEL":
		return s.handleRequestMDel(r, d)
	default:
		return d.dispatch(r)
	}
}

func (s *Session) handleQuit(r *Request) error {
	s.quit = true
	r.Resp = RespOK
	return nil
}

func (s *Session) handleAuth(r *Request) error {
	if len(r.Multi) != 2 {
		r.Resp = redis.NewErrorf("ERR wrong number of arguments for 'AUTH' command")
		return nil
	}
	switch {
	case s.auth == "":
		r.Resp = redis.NewErrorf("ERR Client sent AUTH, but no password is set")
	case s.auth != string(r.Multi[1].Value):
		s.authorized = false
		r.Resp = redis.NewErrorf("ERR invalid password")
	default:
		s.authorized = true
		r.Resp = RespOK
	}
	return nil
}

func (s *Session) handleSelect(r *Request) error {
	if len(r.Multi) != 2 {
		r.Resp = redis.NewErrorf("ERR wrong number of arguments for 'SELECT' command")
		return nil
	}
	switch db, err := strconv.Atoi(string(r.Multi[1].Value)); {
	case err != nil:
		r.Resp = redis.NewErrorf("ERR invalid DB index")
	case db != 0:
		r.Resp = redis.NewErrorf("ERR invalid DB index, only accept DB 0")
	default:
		r.Resp = RespOK
	}
	return nil
}

func (s *Session) handlePing(r *Request) error {
	if len(r.Multi) != 1 {
		r.Resp = redis.NewErrorf("ERR wrong number of arguments for 'PING' command")
	} else {
		r.Resp = redis.NewString([]byte("PONG"))
	}
	return nil
}

func (s *Session) handleRequestMGet(r *Request, d *Router) error {
	var nkeys = len(r.Multi) - 1
	switch {
	case nkeys == 0:
		r.Resp = redis.NewErrorf("ERR wrong number of arguments for 'MGET' command")
		return nil
	case nkeys == 1:
		return d.dispatch(r)
	}
	var sub = make([]*Request, nkeys)
	for i := 0; i < len(sub); i++ {
		sub[i] = s.alloc.SubRequest(r)
		sub[i].Multi = []*redis.Resp{
			r.Multi[0],
			r.Multi[i+1],
		}
		if err := d.dispatch(sub[i]); err != nil {
			return err
		}
	}
	r.Coalesce = func() error {
		var array = make([]*redis.Resp, len(sub))
		for i, x := range sub {
			if err := x.Err; err != nil {
				return err
			}
			switch resp := x.Resp; {
			case resp == nil:
				return ErrRespIsRequired
			case resp.IsArray() && len(resp.Array) == 1:
				array[i] = resp.Array[0]
			default:
				return fmt.Errorf("bad mget resp: %s array.len = %d", resp.Type, len(resp.Array))
			}
		}
		r.Resp = redis.NewArray(array)
		return nil
	}
	return nil
}

func (s *Session) handleRequestMSet(r *Request, d *Router) error {
	var nblks = len(r.Multi) - 1
	switch {
	case nblks == 0 || nblks%2 != 0:
		r.Resp = redis.NewErrorf("ERR wrong number of arguments for 'MSET' command")
		return nil
	case nblks == 2:
		return d.dispatch(r)
	}
	var sub = make([]*Request, nblks/2)
	for i := 0; i < len(sub); i++ {
		sub[i] = s.alloc.SubRequest(r)
		sub[i].Multi = []*redis.Resp{
			r.Multi[0],
			r.Multi[i*2+1],
			r.Multi[i*2+2],
		}
		if err := d.dispatch(sub[i]); err != nil {
			return err
		}
	}
	r.Coalesce = func() error {
		for _, x := range sub {
			if err := x.Err; err != nil {
				return err
			}
			switch resp := x.Resp; {
			case resp == nil:
				return ErrRespIsRequired
			case resp.IsString():
				r.Resp = resp
			default:
				return fmt.Errorf("bad mset resp: %s value.len = %d", resp.Type, len(resp.Value))
			}
		}
		return nil
	}
	return nil
}

func (s *Session) handleRequestMDel(r *Request, d *Router) error {
	var nkeys = len(r.Multi) - 1
	switch {
	case nkeys == 0:
		r.Resp = redis.NewErrorf("ERR wrong number of arguments for 'DEL' command")
		return nil
	case nkeys == 1:
		return d.dispatch(r)
	}
	var sub = make([]*Request, nkeys)
	for i := 0; i < len(sub); i++ {
		sub[i] = s.alloc.SubRequest(r)
		sub[i].Multi = []*redis.Resp{
			r.Multi[0],
			r.Multi[i+1],
		}
		if err := d.dispatch(sub[i]); err != nil {
			return err
		}
	}
	r.Coalesce = func() error {
		var n int
		for _, x := range sub {
			if err := x.Err; err != nil {
				return err
			}
			switch resp := x.Resp; {
			case resp == nil:
				return ErrRespIsRequired
			case resp.IsInt() && len(resp.Value) == 1:
				if resp.Value[0] != '0' {
					n++
				}
			default:
				return fmt.Errorf("bad mdel resp: %s value.len = %d", resp.Type, len(resp.Value))
			}
		}
		r.Resp = redis.NewInt(strconv.AppendInt(nil, int64(n), 10))
		return nil
	}
	return nil
}

func (s *Session) incrOpTotal() {
	s.stats.total.Incr()
}

func (s *Session) incrOpFails(err error) error {
	incrOpFails()
	return err
}

func (s *Session) incrOpStats(r *Request) {
	e := s.stats.opmap[r.OpStr]
	if e == nil {
		e = &opStats{opstr: r.OpStr}
		s.stats.opmap[r.OpStr] = e
	}
	e.calls.Incr()
	e.usecs.Add(utils.Microseconds() - r.Start)
}

func (s *Session) flushOpStats() {
	incrOpTotal(s.stats.total.Swap(0))
	for _, e := range s.stats.opmap {
		if n := e.calls.Swap(0); n != 0 {
			incrOpStats(e.opstr, n, e.usecs.Swap(0))
		}
	}
	s.stats.flush++
	if (s.stats.flush & 0x4000) != 0 {
		return
	}
	s.stats.opmap = make(map[string]*opStats, 16)
}
