/*
    DNS-over-HTTPS
    Copyright (C) 2017 Star Brilliant <m13253@hotmail.com>

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU Affero General Public License as published
    by the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU Affero General Public License for more details.

    You should have received a copy of the GNU Affero General Public License
    along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"github.com/miekg/dns"
	"../json-dns"
)

type Client struct {
	addr		string
	upstream	string
	udpServer	*dns.Server
	tcpServer	*dns.Server
}

func NewClient(addr, upstream string) (c *Client) {
	c = &Client {
		addr: addr,
		upstream: upstream,
	}
	c.udpServer = &dns.Server {
		Addr: addr,
		Net: "udp",
		Handler: dns.HandlerFunc(c.udpHandlerFunc),
		UDPSize: 4096,
	}
	c.tcpServer = &dns.Server {
		Addr: addr,
		Net: "tcp",
		Handler: dns.HandlerFunc(c.tcpHandlerFunc),
	}
	return
}

func (c *Client) Start() error {
	result := make(chan error)
	go func() {
		err := c.udpServer.ListenAndServe()
		if err != nil {
			log.Println(err)
		}
		result <- err
	} ()
	go func() {
		err := c.tcpServer.ListenAndServe()
		if err != nil {
			log.Println(err)
		}
		result <- err
	} ()
	err := <-result
	if err != nil {
		return err
	}
	err = <-result
	return err
}

func (c *Client) handlerFunc(w dns.ResponseWriter, r *dns.Msg, isTCP bool) {
	if r.Response == true {
		log.Println("Received a response packet")
		return
	}

	reply := jsonDNS.PrepareReply(r)

	if len(r.Question) != 1 {
		log.Println("Number of questions is not 1")
		reply.Rcode = dns.RcodeFormatError
		w.WriteMsg(reply)
		return
	}
	question := r.Question[0]
	questionName := question.Name
	questionType := ""
	if qtype, ok := dns.TypeToString[question.Qtype]; ok {
		questionType = qtype
	} else {
		questionType = strconv.Itoa(int(question.Qtype))
	}

	fmt.Printf("%s - - [%s] \"%s IN %s\"\n", w.RemoteAddr(), time.Now().Format("02/Jan/2006:15:04:05 -0700"), questionName, questionType)

	requestURL := fmt.Sprintf("%s?name=%s&type=%s", c.upstream, url.QueryEscape(questionName), url.QueryEscape(questionType))

	if r.CheckingDisabled {
		requestURL += "&cd=1"
	}

	udpSize := uint16(512)
	if opt := r.IsEdns0(); opt != nil {
		udpSize = opt.UDPSize()
	}

	ednsClientAddress, ednsClientNetmask := c.findClientIP(w, r)
	if ednsClientAddress != nil {
		requestURL += fmt.Sprintf("&edns_client_subnet=%s/%d", ednsClientAddress.String(), ednsClientNetmask)
	}

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		log.Println(err)
		reply.Rcode = dns.RcodeServerFailure
		w.WriteMsg(reply)
		return
	}
	req.Header.Set("User-Agent", "DNS-over-HTTPS/1.0 (+https://github.com/m13253/dns-over-https)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Println(err)
		reply.Rcode = dns.RcodeServerFailure
		w.WriteMsg(reply)
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
		reply.Rcode = dns.RcodeServerFailure
		w.WriteMsg(reply)
		return
	}

	var respJson jsonDNS.Response
	err = json.Unmarshal(body, &respJson)
	if err != nil {
		log.Println(err)
		reply.Rcode = dns.RcodeServerFailure
		w.WriteMsg(reply)
		return
	}

	fullReply := jsonDNS.Unmarshal(reply, &respJson, udpSize)
	buf, err := fullReply.Pack()
	if err != nil {
		log.Println(err)
		reply.Rcode = dns.RcodeServerFailure
		w.WriteMsg(reply)
		return
	}
	if !isTCP && len(buf) > int(udpSize) {
		fullReply.Truncated = true
		buf, err = fullReply.Pack()
		if err != nil {
			log.Println(err)
			return
		}
		buf = buf[:udpSize]
	}
	w.Write(buf)
}

func (c *Client) udpHandlerFunc(w dns.ResponseWriter, r *dns.Msg) {
	c.handlerFunc(w, r, false)
}

func (c *Client) tcpHandlerFunc(w dns.ResponseWriter, r *dns.Msg) {
	c.handlerFunc(w, r, true)
}

var (
	ipv4Mask24	net.IPMask = net.IPMask { 255, 255, 255, 0 }
	ipv6Mask48	net.IPMask = net.CIDRMask(48, 128)
)

func (c *Client) findClientIP(w dns.ResponseWriter, r *dns.Msg) (ednsClientAddress net.IP, ednsClientNetmask uint8) {
	ednsClientNetmask = 255
	if opt := r.IsEdns0(); opt != nil {
		for _, option := range opt.Option {
			if option.Option() == dns.EDNS0SUBNET {
				edns0Subnet := option.(*dns.EDNS0_SUBNET)
				ednsClientAddress = edns0Subnet.Address
				ednsClientNetmask = edns0Subnet.SourceNetmask
				return
			}
		}
	}
	remoteAddr, err := net.ResolveUDPAddr("udp", w.RemoteAddr().String())
	if err != nil {
		return
	}
	if ip := remoteAddr.IP; jsonDNS.IsGlobalIP(ip) {
		if ipv4 := ip.To4(); ipv4 != nil {
			ednsClientAddress = ipv4.Mask(ipv4Mask24)
			ednsClientNetmask = 24
		} else {
			ednsClientAddress = ip.Mask(ipv6Mask48)
			ednsClientNetmask = 48
		}
	}
	return
}