package proxy

import (
	"bytes"
	cryptoRand "crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LilyPad/GoLilyPad/packet"
	"github.com/LilyPad/GoLilyPad/packet/minecraft"
	mc112 "github.com/LilyPad/GoLilyPad/packet/minecraft/v112"
	mc1121 "github.com/LilyPad/GoLilyPad/packet/minecraft/v1121"
	mc113 "github.com/LilyPad/GoLilyPad/packet/minecraft/v113"
	mc17 "github.com/LilyPad/GoLilyPad/packet/minecraft/v17"
	mc18 "github.com/LilyPad/GoLilyPad/packet/minecraft/v18"
	mc19 "github.com/LilyPad/GoLilyPad/packet/minecraft/v19"
	"github.com/LilyPad/GoLilyPad/server/proxy/api"
	"github.com/LilyPad/GoLilyPad/server/proxy/auth"
	"github.com/LilyPad/GoLilyPad/server/proxy/connect"
	uuid "github.com/satori/go.uuid"
)

type Session struct {
	server               *Server
	conn                 net.Conn
	connCodec            *packet.PacketConnCodec
	pipeline             *packet.PacketPipeline
	outBridge            *SessionOutBridge
	apiSession           api.Session
	compressionThreshold int

	active            bool
	activeServers     map[string]struct{}
	activeServersLock sync.Mutex

	redirecting   bool
	redirectMutex sync.Mutex

	protocol         *minecraft.Version
	protocolVersion  int
	serverAddress    string
	rawServerAddress string
	name             string
	uuid             uuid.UUID
	profile          auth.GameProfile
	serverId         string
	verifyToken      []byte

	mcBrand        packet.Packet
	clientSettings packet.Packet
	clientEntityId int32
	serverEntityId int32
	pluginChannels map[string]struct{}
	playerList     map[string]struct{}
	scoreboards    map[string]struct{}
	teams          map[string]struct{}
	bossBars       map[string]struct{}

	remoteIp   string
	remotePort string
	state      SessionState
}

func NewSession(server *Server, conn net.Conn) (this *Session) {
	this = new(Session)
	this.server = server
	this.conn = conn
	this.apiSession = &apiSession{this}
	this.compressionThreshold = -1
	this.active = true
	this.activeServers = make(map[string]struct{})
	this.redirecting = false
	this.pluginChannels = make(map[string]struct{})
	this.playerList = make(map[string]struct{})
	this.scoreboards = make(map[string]struct{})
	this.teams = make(map[string]struct{})
	this.bossBars = make(map[string]struct{})
	this.remoteIp, this.remotePort, _ = net.SplitHostPort(conn.RemoteAddr().String())
	this.state = STATE_DISCONNECTED
	return
}

func (this *Session) Serve() {
	this.pipeline = packet.NewPacketPipeline()
	this.pipeline.AddLast("varIntLength", packet.NewPacketCodecVarIntLength())
	this.pipeline.AddLast("registry", minecraft.HandshakePacketServerCodec)
	this.connCodec = packet.NewPacketConnCodec(this.conn, this.pipeline, 20*time.Second)
	event := eventSessionOpen{
		eventSessionCancellable: eventSessionCancellable{eventSession: eventSession{this}},
	}
	eventBus := this.server.apiEventBus
	eventBus.fireEventSession(eventBus.sessionOpen, &event)
	if event.IsCancelled() {
		this.conn.Close()
		return
	}
	this.connCodec.ReadConn(this)
}

func (this *Session) Write(packet packet.Packet) (err error) {
	event := this.server.apiEventBus.fireEventSessionPacket(this, &packet, api.PacketStagePre, api.PacketSubjectClient, api.PacketDirectionWrite)
	if event.IsCancelled() {
		return
	}
	err = this.connCodec.Write(packet)
	if err != nil {
		return
	}
	this.server.apiEventBus.fireEventSessionPacket(this, &packet, api.PacketStageMonitor, api.PacketSubjectClient, api.PacketDirectionWrite)
	return
}

func (this *Session) Redirect(server *connect.Server) {
	event := eventSessionRedirect{
		eventSessionCancellable: eventSessionCancellable{eventSession: eventSession{this}},
		init:       this.state == STATE_INIT,
		serverName: server.Name,
		serverAddr: server.Addr,
	}
	eventBus := this.server.apiEventBus
	eventBus.fireEventSession(eventBus.sessionRedirect, &event)
	if event.IsCancelled() {
		return
	}
	conn, err := net.Dial("tcp", server.Addr)
	if err != nil {
		fmt.Println("Proxy server, name:", this.name, "ip:", this.remoteIp, "failed to redirect:", server.Name, "err:", err)
		if this.Initializing() {
			this.Disconnect("Error: Outbound Connection Mismatch")
		}
		return
	}
	fmt.Println("Proxy server, name:", this.name, "ip:", this.remoteIp, "redirected:", server.Name)
	NewSessionOutBridge(this, server, conn).Serve()
}

func (this *Session) SetAuthenticated(result bool) {
	if !result {
		this.Disconnect("Error: Authentication to Minecraft.net Failed")
		return
	}
	this.uuid, _ = uuid.FromString(FormatUUID(this.profile.Id))
	if this.server.SessionRegistry.HasName(this.name) {
		this.Disconnect(minecraft.Colorize(this.server.localizer.LocaleLoggedIn()))
		return
	}
	if this.server.SessionRegistry.HasUuid(this.uuid) {
		this.Disconnect(minecraft.Colorize(this.server.localizer.LocaleLoggedIn()))
		return
	}
	if this.server.MaxPlayers() > 1 && this.server.SessionRegistry.Len() >= int(this.server.MaxPlayers()) {
		this.Disconnect(minecraft.Colorize(this.server.localizer.LocaleFull()))
		return
	}
	servers := this.server.router.Route(this.serverAddress)
	activeServers := []string{}
	for _, serverName := range servers {
		if !this.server.connect.HasServer(serverName) {
			continue
		}
		activeServers = append(activeServers, serverName)
	}
	if len(activeServers) == 0 {
		this.Disconnect(minecraft.Colorize(this.server.localizer.LocaleOffline()))
		return
	}
	serverName := activeServers[RandomInt(len(activeServers))]
	server := this.server.connect.Server(serverName)
	if server == nil {
		this.Disconnect("Error: Outbound Server Mismatch: " + serverName)
		return
	}
	addResult := this.server.connect.AddLocalPlayer(this.name, this.uuid)
	if addResult == 0 {
		this.Disconnect(minecraft.Colorize(this.server.localizer.LocaleLoggedIn()))
		return
	} else if addResult == -1 {
		this.Disconnect(minecraft.Colorize(this.server.localizer.LocaleLoggedIn()))
		return
	}
	this.SetState(STATE_INIT)
	if this.protocolVersion >= mc19.VersionNum {
		this.SetCompression(256)
	}
	if this.protocolVersion >= 5 {
		this.Write(minecraft.NewPacketClientLoginSuccess(this.protocol.IdMap, FormatUUID(this.profile.Id), this.name))
	} else {
		this.Write(minecraft.NewPacketClientLoginSuccess(this.protocol.IdMap, this.profile.Id, this.name))
	}
	this.pipeline.Replace("registry", this.protocol.PlayServerCodec)
	this.connCodec.SetTimeout(20 * time.Second)
	this.server.SessionRegistry.Register(this)
	this.Redirect(server)
}

func (this *Session) SetEncryption(sharedSecret []byte) (err error) {
	codec, err := packet.NewPacketCodecCfb8(sharedSecret)
	if err != nil {
		return
	}
	this.pipeline.AddBefore("cfb8", "varIntLength", codec)
	return
}

func (this *Session) SetCompression(threshold int) {
	if this.compressionThreshold == threshold {
		return
	}
	this.compressionThreshold = threshold
	registry := this.pipeline.Get("registry")
	if registry == this.protocol.LoginServerCodec {
		this.Write(minecraft.NewPacketClientLoginSetCompression(this.protocol.IdMap, threshold))
	} else if registry == mc18.PlayPacketServerCodec || registry == mc17.PlayPacketServerCodec {
		// FIXME 1.9 does not have set compression during play, so we fix compression at 256
		this.Write(minecraft.NewPacketClientSetCompression(this.protocol.IdMap, threshold))
	}
	if threshold == -1 {
		this.pipeline.Remove("zlib")
		return
	} else {
		codec := packet.NewPacketCodecZlib(threshold)
		if this.pipeline.HasName("zlib") {
			this.pipeline.Replace("zlib", codec)
		} else {
			this.pipeline.AddBefore("zlib", "registry", codec)
		}
	}
}

func (this *Session) Disconnect(reason string) {
	reasonJson, _ := json.Marshal(reason)
	this.DisconnectJson("{\"text\":" + string(reasonJson) + "}")
}

func (this *Session) DisconnectJson(json string) {
	if this.protocol != nil {
		registry := this.pipeline.Get("registry")
		if registry == this.protocol.LoginServerCodec {
			this.Write(minecraft.NewPacketClientLoginDisconnect(this.protocol.IdMap, json))
		} else if registry == this.protocol.PlayServerCodec {
			this.Write(minecraft.NewPacketClientDisconnect(this.protocol.IdMap, json))
		}
	}
	this.conn.Close()
}

func (this *Session) HandlePacket(packet packet.Packet) (err error) {
	event := this.server.apiEventBus.fireEventSessionPacket(this, &packet, api.PacketStagePre, api.PacketSubjectClient, api.PacketDirectionRead)
	if event.IsCancelled() {
		return
	}
	err = this.handlePacket(packet)
	if err != nil {
		return
	}
	this.server.apiEventBus.fireEventSessionPacket(this, &packet, api.PacketStageMonitor, api.PacketSubjectClient, api.PacketDirectionRead)
	return
}

func (this *Session) handlePacket(packet packet.Packet) (err error) {
	switch this.state {
	case STATE_DISCONNECTED:
		if handshakePacket, ok := packet.(*minecraft.PacketServerHandshake); ok {
			this.protocolVersion = handshakePacket.ProtocolVersion
			this.rawServerAddress = handshakePacket.ServerAddress
			idx := strings.Index(this.rawServerAddress, "\x00")
			if idx == -1 {
				this.serverAddress = this.rawServerAddress
			} else {
				this.serverAddress = this.rawServerAddress[:idx]
			}
			this.serverAddress = strings.TrimSuffix(this.serverAddress, ".")
			supportedVersion := false
			for _, version := range minecraft.Versions {
				if version != this.protocolVersion {
					continue
				}
				supportedVersion = true
				break
			}
			if handshakePacket.State == 1 {
				if !supportedVersion {
					this.protocolVersion = minecraft.Versions[0]
				}
				this.pipeline.Replace("registry", minecraft.StatusPacketServerCodec)
				this.SetState(STATE_STATUS)
			} else if handshakePacket.State == 2 {
				if !supportedVersion {
					err = errors.New(fmt.Sprintf("Protocol version does not match: %d", this.protocolVersion))
					return
				}
				if this.protocolVersion >= mc113.VersionNum01 {
					this.protocol = mc113.Version01
				} else if this.protocolVersion >= mc113.VersionNum {
					this.protocol = mc113.Version
				} else if this.protocolVersion >= mc1121.VersionNum02 {
					this.protocol = mc1121.Version02
				} else if this.protocolVersion >= mc1121.VersionNum {
					this.protocol = mc1121.Version
				} else if this.protocolVersion >= mc112.VersionNum {
					this.protocol = mc112.Version
				} else if this.protocolVersion >= mc19.VersionNum01 {
					this.protocol = mc19.Version01
				} else if this.protocolVersion >= mc19.VersionNum {
					this.protocol = mc19.Version
				} else if this.protocolVersion >= mc18.VersionNum {
					this.protocol = mc18.Version
				} else {
					this.protocol = mc17.Version
				}
				this.pipeline.Replace("registry", this.protocol.LoginServerCodec)
				this.SetState(STATE_LOGIN)
			} else {
				err = errors.New("Unexpected state")
				return
			}
		} else {
			err = errors.New("Unexpected packet: handshake expected")
			return
		}
	case STATE_STATUS:
		if _, ok := packet.(*minecraft.PacketServerStatusRequest); ok {
			samplePath := this.server.router.RouteSample(this.serverAddress)
			sampleTxt, sampleErr := ioutil.ReadFile(samplePath)
			sample := make([]map[string]interface{}, 0)
			if sampleErr == nil {
				lines := strings.Split(string(sampleTxt), "\n")
				for _, line := range lines {
					line = strings.Replace(line, "\r", "", -1)
					if len(strings.TrimSpace(line)) == 0 {
						continue
					}
					entry := make(map[string]interface{})
					entry["name"] = minecraft.Colorize(line)
					entry["id"] = "00000000-0000-0000-0000-000000000000"
					sample = append(sample, entry)
				}
			}
			icons := this.server.router.RouteIcons(this.serverAddress)
			iconPath := icons[RandomInt(len(icons))]
			icon, iconErr := ioutil.ReadFile(iconPath)
			var iconString string
			if iconErr == nil {
				iconString = "data:image/png;base64," + base64.StdEncoding.EncodeToString(icon)
			}
			version := make(map[string]interface{})
			version["name"] = minecraft.STRING_VERSION
			version["protocol"] = this.protocolVersion
			players := make(map[string]interface{})
			if this.server.SyncMaxPlayers() {
				players["max"] = this.server.connect.MaxPlayers()
			} else {
				players["max"] = this.server.MaxPlayers()
			}
			players["online"] = this.server.connect.Players()
			players["sample"] = sample
			description := make(map[string]interface{})
			motds := this.server.router.RouteMotds(this.serverAddress)
			motd := motds[RandomInt(len(motds))]
			description["text"] = minecraft.Colorize(motd)
			response := make(map[string]interface{})
			response["version"] = version
			response["players"] = players
			response["description"] = description
			if iconString != "" {
				response["favicon"] = iconString
			}

			response["modinfo"] = struct {
				Type    string        `json:"type"`
				ModList []interface{} `json:"modList"`
			}{"FML", []interface{}{}}

			var marshalled []byte
			marshalled, err = json.Marshal(response)
			if err != nil {
				return
			}
			err = this.Write(minecraft.NewPacketClientStatusResponse(string(marshalled)))
			if err != nil {
				return
			}
			this.SetState(STATE_STATUS_PING)
		} else {
			err = errors.New("Unexpected packet: status request expected")
			return
		}
	case STATE_STATUS_PING:
		if statusPing, ok := packet.(*minecraft.PacketServerStatusPing); ok {
			err = this.Write(minecraft.NewPacketClientStatusPing(statusPing.Time))
			if err != nil {
				return
			}
			this.conn.Close()
		} else {
			err = errors.New("Unexpected packet: status ping expected")
			return
		}
	case STATE_LOGIN:
		if loginStart, ok := packet.(*minecraft.PacketServerLoginStart); ok {
			this.name = loginStart.Name
			if len(this.name) > 16 {
				err = errors.New("Unexpected name: length is more than 16")
				return
			}
			if ok, _ := regexp.MatchString("^[a-zA-Z0-9_]+$", this.name); !ok {
				err = errors.New("Unexpected name: pattern mismatch")
				return
			}
			if this.server.Authenticate() {
				this.serverId, err = GenSalt()
				if err != nil {
					return
				}
				this.verifyToken, err = RandomBytes(4)
				if err != nil {
					return
				}
				err = this.Write(minecraft.NewPacketClientLoginEncryptRequest(this.protocol.IdMap, this.serverId, this.server.publicKey, this.verifyToken))
				if err != nil {
					return
				}
				this.SetState(STATE_LOGIN_ENCRYPT)
			} else {
				this.profile = auth.GameProfile{
					Id:         GenNameUUID("OfflinePlayer:" + this.name),
					Properties: make([]auth.GameProfileProperty, 0),
				}
				this.SetAuthenticated(true)
			}
		} else {
			err = errors.New("Unexpected packet: login start expected")
			return
		}
	case STATE_LOGIN_ENCRYPT:
		if loginEncryptResponse, ok := packet.(*minecraft.PacketServerLoginEncryptResponse); ok {
			var sharedSecret []byte
			sharedSecret, err = rsa.DecryptPKCS1v15(cryptoRand.Reader, this.server.privateKey, loginEncryptResponse.SharedSecret)
			if err != nil {
				return
			}
			var verifyToken []byte
			verifyToken, err = rsa.DecryptPKCS1v15(cryptoRand.Reader, this.server.privateKey, loginEncryptResponse.VerifyToken)
			if err != nil {
				return
			}
			if bytes.Compare(this.verifyToken, verifyToken) != 0 {
				err = errors.New("Verify token does not match")
				return
			}
			err = this.SetEncryption(sharedSecret)
			if err != nil {
				return
			}
			var authErr error
			this.profile, authErr = auth.Authenticate(this.name, this.serverId, sharedSecret, this.server.publicKey)
			if authErr != nil {
				this.SetAuthenticated(false)
				fmt.Println("Proxy server, failed to authorize:", this.name, "ip:", this.remoteIp, "err:", authErr)
			} else {
				this.SetAuthenticated(true)
				fmt.Println("Proxy server, authorized:", this.name, "ip:", this.remoteIp)
			}
		} else {
			err = errors.New("Unexpected packet: login encrypt expected")
			return
		}
	case STATE_CONNECTED:
		if _, ok := packet.(*minecraft.PacketServerClientSettings); ok {
			this.clientSettings = packet
		} else if pluginMessage, ok := packet.(*minecraft.PacketServerPluginMessage); ok {
			if pluginMessage.Channel == "REGISTER" {
				channelBytesSplit := bytes.Split(pluginMessage.Data[:], []byte{0})
				if len(channelBytesSplit) >= 128 || len(this.pluginChannels) >= 128 {
					break
				}
				for _, channelBytes := range channelBytesSplit {
					channel := string(channelBytes)
					if _, ok := this.pluginChannels[channel]; ok {
						continue
					}
					if len(this.pluginChannels) >= 128 {
						break
					}
					this.pluginChannels[channel] = struct{}{}
				}
			} else if pluginMessage.Channel == "UNREGISTER" {
				for _, channelBytes := range bytes.Split(pluginMessage.Data[:], []byte{0}) {
					channel := string(channelBytes)
					delete(this.pluginChannels, channel)
				}
			} else if pluginMessage.Channel == "MC|Brand" {
				this.mcBrand = packet
			}
		}
		if this.redirecting {
			break
		}
		if genericPacket, ok := packet.(*minecraft.PacketGeneric); ok {
			genericPacket.SwapEntities(this.clientEntityId, this.serverEntityId, false)
		}
		this.outBridge.Write(packet)
	}
	return
}

func (this *Session) ErrorCaught(err error) {
	if this.Authenticated() {
		this.server.connect.RemoveLocalPlayer(this.name, this.uuid)
		this.server.SessionRegistry.Unregister(this)
		if this.outBridge == nil {
			fmt.Println("Proxy server, name:", this.name, "ip:", this.remoteIp, "disconnected, err:", err)
		} else {
			fmt.Println("Proxy server, name:", this.name, "ip:", this.remoteIp, "disconnected, err:", err, "outBridgeErr:", this.outBridge.disconnectErr)
		}
	}

	this.SetState(STATE_DISCONNECTED)
	this.conn.Close()
	event := eventSessionClose{
		eventSession: eventSession{this},
	}
	eventBus := this.server.apiEventBus
	eventBus.fireEventSession(eventBus.sessionClose, &event)
	return
}

func (this *Session) SetState(state SessionState) {
	this.state = state
	event := eventSessionState{
		eventSession: eventSession{this},
		state:        api.SessionState(this.state),
	}
	eventBus := this.server.apiEventBus
	eventBus.fireEventSession(eventBus.sessionState, &event)
}

func (this *Session) Remote() (ip string, port string) {
	return this.remoteIp, this.remotePort
}

func (this *Session) RemoteOverride(ip string, port string) {
	this.remoteIp = ip
	this.remotePort = port
}

func (this *Session) Authenticated() (val bool) {
	val = this.state == STATE_INIT || this.state == STATE_CONNECTED
	return
}

func (this *Session) Initializing() (val bool) {
	val = this.state == STATE_INIT
	return
}
