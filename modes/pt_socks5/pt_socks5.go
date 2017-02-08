/*
 * Copyright (c) 2014-2015, Yawning Angel <yawning at torproject dot org>
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *    this list of conditions and the following disclaimer.
 *
 *  * Redistributions in binary form must reproduce the above copyright notice,
 *    this list of conditions and the following disclaimer in the documentation
 *    and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

// Go language Tor Pluggable Transport suite.  Works only as a managed
// client/server.
package pt_socks5

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"

	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/log"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/socks5"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/termmon"
	"github.com/OperatorFoundation/shapeshifter-ipc"
	"github.com/OperatorFoundation/shapeshifter-transports/transports/base"
)

const (
	socksAddr = "127.0.0.1:0"
)

var stateDir string

func ClientSetup(termMon *termmon.TermMonitor, target string, ptClientProxy *url.URL, factories map[string]base.ClientFactory) (launched bool, listeners []net.Listener) {
	// Launch each of the client listeners.
	for name, f := range factories {
		ln, err := net.Listen("tcp", socksAddr)
		if err != nil {
			pt.CmethodError(name, err.Error())
			continue
		}

		go clientAcceptLoop(target, termMon, name, f, ln, ptClientProxy)
		pt.Cmethod(name, socks5.Version(), ln.Addr())

		log.Infof("%s - registered listener: %s", name, ln.Addr())

		listeners = append(listeners, ln)
		launched = true
	}
	pt.CmethodsDone()

	return
}

func clientAcceptLoop(target string, termMon *termmon.TermMonitor, name string, f base.ClientFactory, ln net.Listener, proxyURI *url.URL) error {
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				return err
			}
			continue
		}
		go clientHandler(target, termMon, name, f, conn, proxyURI)
	}
}

func clientHandler(target string, termMon *termmon.TermMonitor, name string, f base.ClientFactory, conn net.Conn, proxyURI *url.URL) {
	defer conn.Close()
	termMon.OnHandlerStart()
	defer termMon.OnHandlerFinish()

	// Read the client's SOCKS handshake.
	socksReq, err := socks5.Handshake(conn)
	if err != nil {
		log.Errorf("%s - client failed socks handshake: %s", name, err)
		return
	}
	addrStr := log.ElideAddr(socksReq.Target)

	// Deal with arguments.
	// args, err := f.ParseArgs(&socksReq.Args)
	// if err != nil {
	// 	log.Errorf("%s(%s) - invalid arguments: %s", name, addrStr, err)
	// 	socksReq.Reply(socks5.ReplyGeneralFailure)
	// 	return
	// }

	// Obtain the proxy dialer if any, and create the outgoing TCP connection.
	// dialFn := proxy.Direct.Dial
	// if proxyURI != nil {
	// 	dialer, err := proxy.FromURL(proxyURI, proxy.Direct)
	// 	if err != nil {
	// 		// This should basically never happen, since config protocol
	// 		// verifies this.
	// 		log.Errorf("%s(%s) - failed to obtain proxy dialer: %s", name, addrStr, log.ElideError(err))
	// 		socksReq.Reply(socks5.ReplyGeneralFailure)
	// 		return
	// 	}
	// 	dialFn = dialer.Dial
	// }
	//
	// fmt.Println("Got dialer", dialFn, proxyURI, proxy.Direct)

	remote := f(socksReq.Target)
	if err != nil {
		log.Errorf("%s(%s) - outgoing connection failed: %s", name, addrStr, log.ElideError(err))
		socksReq.Reply(socks5.ErrorToReplyCode(err))
		return
	}
	defer remote.Close()
	err = socksReq.Reply(socks5.ReplySucceeded)
	if err != nil {
		log.Errorf("%s(%s) - SOCKS reply failed: %s", name, addrStr, log.ElideError(err))
		return
	}

	if err = copyLoop(conn, remote); err != nil {
		log.Warnf("%s(%s) - closed connection: %s", name, addrStr, log.ElideError(err))
	} else {
		log.Infof("%s(%s) - closed connection", name, addrStr)
	}

	return
}

func ServerSetup(termMon *termmon.TermMonitor, bindaddrString string, factories map[string]base.ServerFactory, ptServerInfo pt.ServerInfo) (launched bool, listeners []base.TransportListener) {
	for _, bindaddr := range ptServerInfo.Bindaddrs {
		name := bindaddr.MethodName
		f := factories[name]
		if f == nil {
			fmt.Println(name, "no such transport is supported")
			continue
		}

		transportLn := f(bindaddr.Addr.String())

		go serverAcceptLoop(termMon, name, transportLn, &ptServerInfo)

		// if args := f.Args(); args != nil {
		// 	pt.SmethodArgs(name, ln.Addr(), *args)
		// } else {
		// 	pt.SmethodArgs(name, ln.Addr(), nil)
		// }

		log.Infof("%s - registered listener: %s", name, log.ElideAddr(bindaddr.Addr.String()))

		listeners = append(listeners, transportLn)
		launched = true
	}
	pt.SmethodsDone()

	return
}

func serverAcceptLoop(termMon *termmon.TermMonitor, name string, ln base.TransportListener, info *pt.ServerInfo) error {
	defer ln.Close()
	for {
		conn, err := ln.TransportAccept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				return err
			}
			continue
		}
		go serverHandler(termMon, name, conn, info)
	}
}

func serverHandler(termMon *termmon.TermMonitor, name string, remote base.TransportConn, info *pt.ServerInfo) {
	defer remote.NetworkConn().Close()
	termMon.OnHandlerStart()
	defer termMon.OnHandlerFinish()

	addrStr := log.ElideAddr(remote.NetworkConn().RemoteAddr().String())
	log.Infof("%s(%s) - new connection", name, addrStr)

	// Connect to the orport.
	orConn, err := pt.DialOr(info, remote.NetworkConn().RemoteAddr().String(), name)
	if err != nil {
		log.Errorf("%s(%s) - failed to connect to ORPort: %s", name, addrStr, log.ElideError(err))
		return
	}
	defer orConn.Close()

	if err = copyLoop(orConn, remote); err != nil {
		log.Warnf("%s(%s) - closed connection: %s", name, addrStr, log.ElideError(err))
	} else {
		log.Infof("%s(%s) - closed connection", name, addrStr)
	}

	return
}

func copyLoop(a net.Conn, b net.Conn) error {
	// Note: b is always the pt connection.  a is the SOCKS/ORPort connection.
	errChan := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer b.Close()
		defer a.Close()
		_, err := io.Copy(b, a)
		errChan <- err
	}()
	go func() {
		defer wg.Done()
		defer a.Close()
		defer b.Close()
		_, err := io.Copy(a, b)
		errChan <- err
	}()

	// Wait for both upstream and downstream to close.  Since one side
	// terminating closes the other, the second error in the channel will be
	// something like EINVAL (though io.Copy() will swallow EOF), so only the
	// first error is returned.
	wg.Wait()
	if len(errChan) > 0 {
		return <-errChan
	}

	return nil
}
