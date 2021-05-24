package core

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	//"sync/atomic"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/util"
	"golang.org/x/net/proxy"
	//"github.com/Arceliar/phony" // TODO? use instead of mutexes
)

type keyArray [ed25519.PublicKeySize]byte

type links struct {
	core    *Core
	mutex   sync.RWMutex // protects links below
	links   map[linkInfo]*link
	tcp     tcp // TCP interface support
	stopped chan struct{}
	// TODO timeout (to remove from switch), read from config.ReadTimeout
}

// linkInfo is used as a map key
type linkInfo struct {
	key      keyArray
	linkType string // Type of link, e.g. TCP, AWDL
	local    string // Local name or address
	remote   string // Remote name or address
}

type link struct {
	lname    string
	links    *links
	conn     net.Conn
	options  linkOptions
	info     linkInfo
	incoming bool
	force    bool
	closed   chan struct{}
}

type linkOptions struct {
	pinnedEd25519Keys map[keyArray]struct{}
	metric            uint8
}

func (l *links) init(c *Core) error {
	l.core = c
	l.mutex.Lock()
	l.links = make(map[linkInfo]*link)
	l.mutex.Unlock()
	l.stopped = make(chan struct{})

	if err := l.tcp.init(l); err != nil {
		c.log.Errorln("Failed to start TCP interface")
		return err
	}

	return nil
}

func (l *links) reconfigure() {
	l.tcp.reconfigure()
}

func (l *links) call(u *url.URL, sintf string) error {
	//u, err := url.Parse(uri)
	//if err != nil {
	//	return fmt.Errorf("peer %s is not correctly formatted (%s)", uri, err)
	//}
	tcpOpts := tcpOptions{}
	if pubkeys, ok := u.Query()["ed25519"]; ok && len(pubkeys) > 0 {
		tcpOpts.pinnedEd25519Keys = make(map[keyArray]struct{})
		for _, pubkey := range pubkeys {
			if sigPub, err := hex.DecodeString(pubkey); err == nil {
				var sigPubKey keyArray
				copy(sigPubKey[:], sigPub)
				tcpOpts.pinnedEd25519Keys[sigPubKey] = struct{}{}
			}
		}
	}
	if ms := u.Query()["metric"]; len(ms) == 1 {
		m64, _ := strconv.ParseUint(ms[0], 10, 8)
		tcpOpts.metric = uint8(m64)
	}
	switch u.Scheme {
	case "tcp":
		l.tcp.call(u.Host, tcpOpts, sintf)
	case "socks":
		tcpOpts.socksProxyAddr = u.Host
		if u.User != nil {
			tcpOpts.socksProxyAuth = &proxy.Auth{}
			tcpOpts.socksProxyAuth.User = u.User.Username()
			tcpOpts.socksProxyAuth.Password, _ = u.User.Password()
		}
		pathtokens := strings.Split(strings.Trim(u.Path, "/"), "/")
		l.tcp.call(pathtokens[0], tcpOpts, sintf)
	case "tls":
		tcpOpts.upgrade = l.tcp.tls.forDialer
		l.tcp.call(u.Host, tcpOpts, sintf)
	default:
		return errors.New("unknown call scheme: " + u.Scheme)
	}
	return nil
}

func (l *links) create(conn net.Conn, name, linkType, local, remote string, incoming, force bool, options linkOptions) (*link, error) {
	// Technically anything unique would work for names, but let's pick something human readable, just for debugging
	intf := link{
		conn:    conn,
		lname:   name,
		links:   l,
		options: options,
		info: linkInfo{
			linkType: linkType,
			local:    local,
			remote:   remote,
		},
		incoming: incoming,
		force:    force,
	}
	return &intf, nil
}

func (l *links) stop() error {
	close(l.stopped)
	if err := l.tcp.stop(); err != nil {
		return err
	}
	return nil
}

func (intf *link) handler() (chan struct{}, error) {
	// TODO split some of this into shorter functions, so it's easier to read, and for the FIXME duplicate peer issue mentioned later
	defer intf.conn.Close()
	meta := version_getBaseMetadata()
	meta.key = intf.links.core.public
	meta.metric = intf.options.metric
	metric := uint64(meta.metric)
	metaBytes := meta.encode()
	// TODO timeouts on send/recv (goroutine for send/recv, channel select w/ timer)
	var err error
	if !util.FuncTimeout(30*time.Second, func() {
		var n int
		n, err = intf.conn.Write(metaBytes)
		if err == nil && n != len(metaBytes) {
			err = errors.New("incomplete metadata send")
		}
	}) {
		return nil, errors.New("timeout on metadata send")
	}
	if err != nil {
		return nil, err
	}
	if !util.FuncTimeout(30*time.Second, func() {
		var n int
		n, err = io.ReadFull(intf.conn, metaBytes)
		if err == nil && n != len(metaBytes) {
			err = errors.New("incomplete metadata recv")
		}
	}) {
		return nil, errors.New("timeout on metadata recv")
	}
	if err != nil {
		return nil, err
	}
	meta = version_metadata{}
	base := version_getBaseMetadata()
	if !meta.decode(metaBytes) {
		return nil, errors.New("failed to decode metadata")
	}
	if metric < uint64(meta.metric) {
		metric = uint64(meta.metric)
	}
	if !meta.check() {
		intf.links.core.log.Errorf("Failed to connect to node: %s is incompatible version (local %s, remote %s)",
			intf.lname,
			fmt.Sprintf("%d.%d", base.ver, base.minorVer),
			fmt.Sprintf("%d.%d", meta.ver, meta.minorVer),
		)
		return nil, errors.New("remote node is incompatible version")
	}
	// Check if the remote side matches the keys we expected. This is a bit of a weak
	// check - in future versions we really should check a signature or something like that.
	if pinned := intf.options.pinnedEd25519Keys; pinned != nil {
		var key keyArray
		copy(key[:], meta.key)
		if _, allowed := pinned[key]; !allowed {
			intf.links.core.log.Errorf("Failed to connect to node: %q sent ed25519 key that does not match pinned keys", intf.name)
			return nil, fmt.Errorf("failed to connect: host sent ed25519 key that does not match pinned keys")
		}
	}
	// Check if we're authorized to connect to this key / IP
	allowed := intf.links.core.config.GetCurrent().AllowedPublicKeys
	isallowed := len(allowed) == 0
	for _, k := range allowed {
		if k == hex.EncodeToString(meta.key) { // TODO: this is yuck
			isallowed = true
			break
		}
	}
	if intf.incoming && !intf.force && !isallowed {
		intf.links.core.log.Warnf("%s connection from %s forbidden: AllowedEncryptionPublicKeys does not contain key %s",
			strings.ToUpper(intf.info.linkType), intf.info.remote, hex.EncodeToString(meta.key))
		intf.close()
		return nil, nil
	}
	// Check if we already have a link to this node
	copy(intf.info.key[:], meta.key)
	intf.links.mutex.Lock()
	if oldIntf, isIn := intf.links.links[intf.info]; isIn {
		intf.links.mutex.Unlock()
		// FIXME we should really return an error and let the caller block instead
		// That lets them do things like close connections on its own, avoid printing a connection message in the first place, etc.
		intf.links.core.log.Debugln("DEBUG: found existing interface for", intf.name)
		return oldIntf.closed, nil
	} else {
		intf.closed = make(chan struct{})
		intf.links.links[intf.info] = intf
		defer func() {
			intf.links.mutex.Lock()
			delete(intf.links.links, intf.info)
			intf.links.mutex.Unlock()
			close(intf.closed)
		}()
		intf.links.core.log.Debugln("DEBUG: registered interface for", intf.name)
	}
	intf.links.mutex.Unlock()
	themAddr := address.AddrForKey(ed25519.PublicKey(intf.info.key[:]))
	themAddrString := net.IP(themAddr[:]).String()
	themString := fmt.Sprintf("%s@%s", themAddrString, intf.info.remote)
	intf.links.core.log.Infof("Connected %s: %s, source %s",
		strings.ToUpper(intf.info.linkType), themString, intf.info.local)
	// Run the handler
	err = intf.links.core.PacketConn.HandleConn(ed25519.PublicKey(intf.info.key[:]), intf.conn, metric)
	// TODO don't report an error if it's just a 'use of closed network connection'
	if err != nil {
		intf.links.core.log.Infof("Disconnected %s: %s, source %s; error: %s",
			strings.ToUpper(intf.info.linkType), themString, intf.info.local, err)
	} else {
		intf.links.core.log.Infof("Disconnected %s: %s, source %s",
			strings.ToUpper(intf.info.linkType), themString, intf.info.local)
	}
	return nil, err
}

func (intf *link) close() {
	intf.conn.Close()
}

func (intf *link) name() string {
	return intf.lname
}

func (intf *link) local() string {
	return intf.info.local
}

func (intf *link) remote() string {
	return intf.info.remote
}

func (intf *link) interfaceType() string {
	return intf.info.linkType
}