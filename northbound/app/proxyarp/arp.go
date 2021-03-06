/*
 * Cherry - An OpenFlow Controller
 *
 * Copyright (C) 2015 Samjung Data Service, Inc. All rights reserved.
 * Kitae Kim <superkkt@sds.co.kr>
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License along
 * with this program; if not, write to the Free Software Foundation, Inc.,
 * 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301 USA.
 */

package proxyarp

import (
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/superkkt/cherry/network"
	"github.com/superkkt/cherry/northbound/app"
	"github.com/superkkt/cherry/northbound/util/announcer"
	"github.com/superkkt/cherry/openflow"
	"github.com/superkkt/cherry/protocol"

	"github.com/pkg/errors"
	"github.com/superkkt/go-logging"
)

var (
	logger = logging.MustGetLogger("proxyarp")
)

type ProxyARP struct {
	app.BaseProcessor
	db   database
	once sync.Once
}

type database interface {
	MAC(ip net.IP) (mac net.HardwareAddr, ok bool, err error)
	GetActivatedHosts() ([]Host, error)
}

type Host struct {
	IP  net.IP
	MAC net.HardwareAddr
}

func New(db database) *ProxyARP {
	return &ProxyARP{
		db: db,
	}
}

func (r *ProxyARP) Init() error {
	return nil
}

func (r *ProxyARP) Name() string {
	return "ProxyARP"
}

func (r *ProxyARP) OnDeviceUp(finder network.Finder, device *network.Device) error {
	// Make sure that there is only one ProxyARP broadcaster in this application.
	r.once.Do(func() {
		// Run the background broadcaster for periodic ARP announcement.
		go r.broadcaster(finder)
	})

	return r.BaseProcessor.OnDeviceUp(finder, device)
}

func (r *ProxyARP) broadcaster(finder network.Finder) {
	logger.Debug("executed ARP announcement broadcaster")

	backoff := announcer.NewBackoffARPAnnouncer(finder)

	ticker := time.Tick(5 * time.Second)
	// Infinite loop.
	for range ticker {
		hosts, err := r.db.GetActivatedHosts()
		if err != nil {
			logger.Errorf("failed to get host addresses: %v", err)
			continue
		}

		for _, v := range hosts {
			logger.Debugf("broadcasting an ARP announcement for a host: IP=%v, MAC=%v", v.IP, v.MAC)

			if err := backoff.Broadcast(v.IP, v.MAC); err != nil {
				logger.Errorf("failed to broadcast an ARP announcement: %v", err)
				continue
			}
			// Sleep to mitigate the peak latency of processing PACKET_INs.
			time.Sleep(time.Duration(10+rand.Intn(100)) * time.Millisecond)
		}
	}
}

func (r *ProxyARP) OnPacketIn(finder network.Finder, ingress *network.Port, eth *protocol.Ethernet) error {
	// ARP?
	if eth.Type != 0x0806 {
		return r.BaseProcessor.OnPacketIn(finder, ingress, eth)
	}

	logger.Debugf("received ARP packet.. ingress=%v, srcEthMAC=%v, dstEthMAC=%v", ingress.ID(), eth.SrcMAC, eth.DstMAC)

	arp := new(protocol.ARP)
	if err := arp.UnmarshalBinary(eth.Payload); err != nil {
		return err
	}
	// Drop ARP announcement
	if isARPAnnouncement(arp) {
		// We don't allow a host sends ARP announcement to the network. This controller only can send it,
		// and we will flood the announcement to all switch devices using PACKET_OUT  when we need it.
		logger.Debugf("drop ARP announcements.. ingress=%v (%v)", ingress.ID(), arp)
		return nil
	}
	// ARP request?
	if arp.Operation != 1 {
		// Drop all ARP packets whose type is not a reqeust.
		logger.Infof("drop ARP packet whose type is not a request.. ingress=%v (%v)", ingress.ID(), arp)
		return nil
	}

	mac, ok, err := r.db.MAC(arp.TPA)
	if err != nil {
		return errors.Wrap(&proxyarpErr{temporary: true, err: err}, "failed to query MAC")
	}
	if !ok {
		logger.Debugf("drop the ARP request for unknown host (%v)", arp.TPA)
		// Unknown hosts. Drop the packet.
		return nil
	}
	logger.Debugf("ARP request for %v (%v)", arp.TPA, mac)

	reply, err := makeARPReply(arp, mac)
	if err != nil {
		return err
	}
	logger.Debugf("sending ARP reply to %v..", ingress.ID())

	return sendARPReply(ingress, reply)
}

func sendARPReply(ingress *network.Port, packet []byte) error {
	f := ingress.Device().Factory()

	inPort := openflow.NewInPort()
	inPort.SetController()

	outPort := openflow.NewOutPort()
	outPort.SetValue(ingress.Number())

	action, err := f.NewAction()
	if err != nil {
		return err
	}
	action.SetOutPort(outPort)

	out, err := f.NewPacketOut()
	if err != nil {
		return err
	}
	out.SetInPort(inPort)
	out.SetAction(action)
	out.SetData(packet)

	return ingress.Device().SendMessage(out)
}

func isARPAnnouncement(request *protocol.ARP) bool {
	sameProtoAddr := request.SPA.Equal(request.TPA)
	sameHWAddr := bytes.Compare(request.SHA, request.THA) == 0
	zeroTarget := bytes.Compare(request.THA, []byte{0, 0, 0, 0, 0, 0}) == 0
	broadcastTarget := bytes.Compare(request.THA, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) == 0
	if sameProtoAddr && (zeroTarget || broadcastTarget || sameHWAddr) {
		return true
	}

	return false
}

func makeARPReply(request *protocol.ARP, mac net.HardwareAddr) ([]byte, error) {
	v := protocol.NewARPReply(mac, request.SHA, request.TPA, request.SPA)
	reply, err := v.MarshalBinary()
	if err != nil {
		return nil, err
	}
	eth := protocol.Ethernet{
		SrcMAC:  mac,
		DstMAC:  request.SHA,
		Type:    0x0806,
		Payload: reply,
	}

	return eth.MarshalBinary()
}

func (r *ProxyARP) String() string {
	return fmt.Sprintf("%v", r.Name())
}
