package gossiper

import (
	"encoding/hex"
	"fmt"
	. "github.com/SubutaiBogatur/Peerster/config"
	. "github.com/SubutaiBogatur/Peerster/models"
	. "github.com/SubutaiBogatur/Peerster/models/blockchain"
	. "github.com/SubutaiBogatur/Peerster/models/filesearching"
	. "github.com/SubutaiBogatur/Peerster/models/filesharing"
	. "github.com/SubutaiBogatur/Peerster/utils"
	"github.com/dedis/protobuf"
	log "github.com/sirupsen/logrus"
	"math/rand"
	. "net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Some documentation on what's happening
//
// We will have threads:
// * client-reader          thread : the only port reading from client socket. Sends client message to message-processor
// * peer-reader            thread : the only port reading from peer socket. Sends GossipPackets to message-processor
// * peer-writer            thread : the only port writing to peer socket. Listens to channel for GossipPackets and writes them
// * anti-entropy-timer     thread : goroutine sends a status to random peer every timeout seconds
// * route-rumoring         thread : once in a timer sends empty rumor message to random peers, so everyone will know about this origin (accesses nothing)
// * message-processor      thread : is an abstraction on client-listener, peer-listener threads. Receives all the messages and for every message:
//     + rumor-msg      : upd status, send status back, send rumor randomly further, start rumor-mongering thread waiting for status
//     + status-msg     : push it to one of the rumor-mongering threads (if rumor mongering is not in progress, then compare statuses and start it)
//     + private        : ezy - forward if needed, else display
//     + data-request   : just answer with needed data, no state saved
//     + data-reply     : answer with next request (if needed) and start file-downloading thread to wait for next data-reply or timeout
//     + search-request : answer with needed data & start search-reply-timeout thread not to answer this request once again
//     + search-reply   : put message via channel (via struct) to the only search-request thread
// * rumor-mongering        thread : thread waits either for status-msg to arrive or for timeout and stores rumor-msg, it was initiated for
//     + status-msg : cmp (store sync-safe Map for VectorClock, which are edited from message-processor) and send new msg via peer-communicator
//     + timeout    : 1/2 & send new rumor-msg via peer-communicator
// * webserver              thread : listens to http-requests on a given port and reads / writes from gossiper object
// * file-downloading       thread : thread waits either for DataReply msg from fixed origin or for timeout
// * search-request-timeout thread : we don't answer the same search-request for some time after we answered it
// * search-request         thread : the only goroutine, which maintains current search-request: reads search-replies and repeats search-requests with more budget
// * mining                 thread : all the time, when exists pending tx, tries to generate new block and then publishes it

var (
	clientMessagesToProcess = make(chan *ClientMessage)
	peerMessagesToProcess   = make(chan *AddressedGossipPacket)
	peerMessagesToSend      = make(chan *AddressedGossipPacket)

	// map is accessed from message-processor and from rumor-mongering threads, should be converted to sync.Map{}
	statusesChannels    = make(map[string]chan *StatusPacket) // peerIp -> channel, where rumor-mongering goroutine is waiting for status feedback
	statusesChannelsMux sync.Mutex

	// accessed from message-processor and from file-downloading threads
	downloadingFilesChannels    = make(map[string]chan *DataReply)
	downloadingFilesChannelsMux sync.Mutex
)

type Gossiper struct {
	name atomic.Value // name can be changed by webserver and is accessed from message-processor, thus using atomic string

	peersAddress     *UDPAddr // peersAddress for peers
	peersConnection  *UDPConn
	clientAddress    *UDPAddr
	clientConnection *UDPConn

	// peers can be understood as "neighbours". Provided from initalization + web + relay rumor addresses
	peers         []*UDPAddr // accessed eg from message-processor and from rumor-mongering
	peersSliceMux sync.Mutex

	// keys are origins. Can be understood as gossipers, who initiated messages we received (not relay!)
	// values are peers, from whom we received the latest message from the origin
	nextHop    map[string]*UDPAddr // accessed from message-processor and from webserver (for keys)
	nextHopMux sync.Mutex

	messageStorage          *MessageStorage          // accessed eg from message-processor and from rumor-mongering, is hard-synchronized
	sharedFilesManager      *SharedFilesManager      // accessed eg from message-processor and from search-request, is hard-synchronized
	downloadingFilesManager *DownloadingFilesManager // accessed eg from message-processor and from file-downloading, is soft-synchronized
	blockchainManager       *BlockchainManager       // accessed eg from message-processor and from mining-thread, is hard-synchronized

	currentSearchRequest *CurrentSearchRequest

	recentSearchRequestsMux sync.Mutex
	recentSearchRequests    map[string]bool // set of recently answered search-requests, don't answer them now, key = "{origin}-{keywords separated with coma}"

	isSimpleMode bool       // in simple mode sending only simple messages
	l            *log.Entry // logger
}

func NewGossiper(name string, uiport int, peersAddress string, peers string, isSimpleMode bool) (*Gossiper, error) {
	logger := log.WithField("bin", "gos").WithField("name", name)

	g := &Gossiper{}
	g.messageStorage = InitMessageStorage(name)
	g.sharedFilesManager = InitSharedFilesManager(logger)
	g.downloadingFilesManager = InitDownloadingFilesManager(logger)
	g.blockchainManager = InitBlockchainManager(logger)
	g.nextHop = make(map[string]*UDPAddr)
	g.recentSearchRequests = make(map[string]bool)
	g.l = logger
	g.isSimpleMode = isSimpleMode

	clientListenAddress := LocalIp + ":" + strconv.Itoa(uiport)
	clientListenUdpAddress, err := ResolveUDPAddr("udp4", clientListenAddress)
	if err != nil {
		g.l.Error("Unable to parse clientListenAddress: " + string(clientListenAddress))
		return g, err
	}
	g.clientAddress = clientListenUdpAddress

	peersListenUdpAddress, err := ResolveUDPAddr("udp4", peersAddress)
	if err != nil {
		g.l.Error("Unable to parse peersListenAddress: " + string(peersAddress))
		return g, err
	}
	g.peersAddress = peersListenUdpAddress
	g.l = g.l.WithField("a", peersListenUdpAddress.String())

	if name == "" {
		return g, PeersterError{ErrorMsg: "Empty name provided"}
	}
	g.name.Store(name)

	peersArr := strings.Split(peers, ",")
	for i := 0; i < len(peersArr); i++ {
		if peersArr[i] == "" {
			continue
		}

		peerAddr, err := ResolveUDPAddr("udp4", peersArr[i])
		if err != nil {
			g.l.Error("Error when parsing peers")
			return g, err
		}
		g.peers = append(g.peers, peerAddr)
	}

	// command line arguments parsed, start listening:
	peersUdpConn, err := ListenUDP("udp4", g.peersAddress)
	if err != nil {
		return g, err
	}
	g.peersConnection = peersUdpConn

	clientUdpConn, err := ListenUDP("udp4", g.clientAddress)
	if err != nil {
		return g, err
	}
	g.clientConnection = clientUdpConn

	return g, nil
}

//helper functions:
func (g *Gossiper) getRandomPeer() *UDPAddr {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	randomInt := rand.Int31n(int32(len(g.peers))) // end not inclusive
	randomPeer := g.peers[randomInt]

	return randomPeer
}

func (g *Gossiper) arePeersEmpty() bool {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	return len(g.peers) == 0
}

func (g *Gossiper) sendAllPeers(gp *GossipPacket) {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	for _, p := range g.peers {
		agp := &AddressedGossipPacket{Packet: gp, Address: p}
		peerMessagesToSend <- agp
	}
}

func (g *Gossiper) GetPeersCopy() []*UDPAddr {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	copySlice := make([]*UDPAddr, len(g.peers))
	copy(copySlice, g.peers)

	return copySlice
}

func (g *Gossiper) UpdatePeersIfNeeded(peer *UDPAddr) {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	for i := 0; i < len(g.peers); i++ {
		if g.peers[i].String() == peer.String() {
			return // peer is a known one
		}
	}
	g.peers = append(g.peers, peer)
}

func (g *Gossiper) printPeers() {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	fmt.Print("PEERS ")
	for i := 0; i < len(g.peers); i++ {
		if i == len(g.peers)-1 {
			fmt.Print(g.peers[i].String())
		} else {
			fmt.Print(g.peers[i].String() + ",")
		}
	}
	fmt.Println("")
}

func (g *Gossiper) GetOriginsCopy() *[]string {
	g.nextHopMux.Lock()
	defer g.nextHopMux.Unlock()

	origins := make([]string, 0, len(g.nextHop))
	for k := range g.nextHop {
		origins = append(origins, k) // todo: len + append, seems like bug
	}

	sort.Strings(origins)

	return &origins
}

func (g *Gossiper) GetSharedFiles() []string {
	return g.sharedFilesManager.GetSharedFilesList()
}

// returns matches for previous search request
func (g *Gossiper) GetFullSearchMatches() []string {
	if g.currentSearchRequest == nil {
		return make([]string, 0)
	}
	return g.currentSearchRequest.GetFullMatches()
}

func (g *Gossiper) DownloadFoundByName(name string) {
	g.currentSearchRequest.DownloadFileByName(name, name)
}

func (g *Gossiper) GetName() string {
	return g.name.Load().(string)
}

func (g *Gossiper) SetName(name string) {
	g.name.Store(name)
}

func (g *Gossiper) GetID(name string) uint32 {
	return g.messageStorage.GetNextMessageId(name)
}

func (g *Gossiper) GetPeerAddress() *UDPAddr {
	return g.peersAddress
}

func (g *Gossiper) GetClientAddress() *UDPAddr {
	return g.clientAddress
}

func (g *Gossiper) GetRumorMessages() *[]RumorMessage {
	return g.messageStorage.GetRumorMessagesCopy()
}

func (g *Gossiper) GetPrivateMessages() *[]PrivateMessage {
	return g.messageStorage.GetPrivateMessagesCopy()
}

func (g *Gossiper) updateNextHop(origin string, relay *UDPAddr) {
	g.nextHopMux.Lock()
	defer g.nextHopMux.Unlock()
	addr, ok := g.nextHop[origin]
	if !ok || ok && addr.String() != relay.String() {
		fmt.Println("DSDV " + origin + " " + relay.String())
		g.nextHop[origin] = relay
	}
}

func (g *Gossiper) sendPacketWithNextHop(origin string, gp *GossipPacket) {
	g.nextHopMux.Lock()
	defer g.nextHopMux.Unlock()
	if g.nextHop[origin] != nil {
		g.l.Debug("sending packet with next hop to "+origin+", next hop appeared to be: "+g.nextHop[origin].String()+" whole map is ", g.nextHop)
		agp := &AddressedGossipPacket{Address: g.nextHop[origin], Packet: gp}
		peerMessagesToSend <- agp
	} else {
		g.l.Warn("unable to send packet, because unknown origin in nextHop function")
	}
}

// client-reader thread
func (g *Gossiper) StartClientReader() {
	g.l.Info("starting reading bytes on client: " + g.clientAddress.String() + " thread")

	for {
		buffer := make([]byte, MaxPacketSize)
		g.clientConnection.ReadFromUDP(buffer)

		cmsg := &ClientMessage{}
		if err := protobuf.Decode(buffer, cmsg); err != nil {
			// todo(atukallo): fix some protobuf warnings
			g.l.Warn("unable to decode message, error: " + err.Error())
		}

		// ~~~ put into channel ~~~
		g.l.Debug("put client message into channel")
		clientMessagesToProcess <- cmsg
		// ~~~~~~~~~~~~~~~~~~~~~~~~
	}
}

// peer-reader thread
func (g *Gossiper) StartPeerReader() {
	g.l.Info("gossiper " + g.name.Load().(string) + ": starting reading bytes on peer: " + g.peersAddress.String() + " in new thread")

	for {
		buffer := make([]byte, MaxPacketSize)
		// here was a terrible bug
		// before buffer was initalized outside the for-loop, then very strange problems were encountered
		// thanks to akashuba it was understood, that protobuf, when doing Decoding, saves in inner structures
		// pointers to buffer. Eg structures somewhere have slices and these slices are not copies of buffer, but pointers
		// to buffer. As a result, with new message arriving, all the previous structures broke down. Now buffer
		// is initialized inside and every iteration it's a new variable, which will never be spoiled later
		_, addr, _ := g.peersConnection.ReadFromUDP(buffer)

		gp := &GossipPacket{}
		if err := protobuf.Decode(buffer, gp); err != nil {
			// todo(atukallo): fix some protobuf warnings
			g.l.Warn("unable to decode message, error: " + err.Error())
		}

		apg := &AddressedGossipPacket{Address: addr, Packet: gp}

		// ~~~ put into channel ~~~
		g.l.Debug("put peer message into channel")
		peerMessagesToProcess <- apg
		// ~~~~~~~~~~~~~~~~~~~~~~~~
	}
}

// peer-writer thread
func (g *Gossiper) StartPeerWriter() {
	g.l.Info("starting peer writer thread")

	for {
		// ~~~ read from channel ~~~
		agp := <-peerMessagesToSend
		// ~~~~~~~~~~~~~~~~~~~~~~~~~

		address := agp.Address
		gp := agp.Packet

		packetBytes, err := protobuf.Encode(gp)
		if err != nil {
			g.l.Error("unable to encode msg: " + err.Error())
			continue
		}

		g.l.Debug("sending message from " + g.peersConnection.LocalAddr().String() + " to " + address.String())

		n, err := g.peersConnection.WriteToUDP(packetBytes, address)
		if err != nil {
			g.l.Error("error when writing to connection: " + err.Error() + " n is " + strconv.Itoa(n))
			continue
		}
	}
}

// anti-entropy thread
func (g *Gossiper) StartAntiEntropyTimer() {
	g.l.Info("starting anti-entropy timer thread")

	for {
		time.Sleep(AntiEntropyTimeout)

		if g.arePeersEmpty() {
			time.Sleep(AntiEntropyTimeout * 5) // wait additional time
			continue
		}

		// send status to a random peer
		peer := g.getRandomPeer()
		peerMessagesToSend <- &AddressedGossipPacket{Address: peer, Packet: &GossipPacket{Status: g.messageStorage.GetCurrentStatusPacket()}}
	}
}

func (g *Gossiper) StartMiningThread() {
	g.l.Info("starting mining thread")

	for {
		g.l.Info("starting mining new block..")
		newblock, timeMining := g.blockchainManager.DoMining()
		if g.blockchainManager.GetBlockNumber() == 1 {
			// then mined block is the first one, hw asks us to sleep more (for fun with pending blocks)
			g.l.Info("sleeping more after first block is mined")
			time.Sleep(5 * time.Second)
		} else {
			// just for a homework to increase number of blocks & forks, sleep before publishing the block and continuing
			time.Sleep(2 * timeMining)
		}

		gp := &GossipPacket{BlockPublish: &BlockPublish{Block: *newblock, HopLimit: BlockchainBlockPublishHopLimit}}
		g.l.Info("sending new block to everyone")
		g.sendAllPeers(gp)
	}
}

// --------------------------------------
// message-processor thread section:
// --------------------------------------

func (g *Gossiper) StartMessageProcessor() {
	g.l.Info("starting message processor")

	for {
		select {
		case cmsg := <-clientMessagesToProcess:
			g.l.Debug("got client message from channel")
			if cmsg.Print() {
				g.printPeers() // if printed something
			}
			g.processClientMessage(cmsg)
		case agp := <-peerMessagesToProcess:
			g.l.Debug("got peer message from channel")
			if agp.Print() {
				g.printPeers() // if printed something
			}
			g.processAddressedGossipPacket(agp)
		}
	}
}

func (g *Gossiper) processClientMessage(cmsg *ClientMessage) {
	gossiperName := g.name.Load().(string)
	if cmsg.Rumor != nil {
		g.l.Info("got client rumor message: " + cmsg.Rumor.Text)
		if g.isSimpleMode {
			g.l.Info("mode is simple, so client rumor message is distributed as simple one to everyone")
			smsg := &SimpleMessage{Text: cmsg.Rumor.Text, OriginalName: gossiperName}
			g.processAddressedSimpleMessage(smsg, nil)
		} else {
			messageId := g.messageStorage.GetNextMessageId(gossiperName)
			rmsg := &RumorMessage{OriginalName: gossiperName, ID: messageId, Text: cmsg.Rumor.Text}
			g.processRumorMessage(rmsg)
		}
	} else if cmsg.RouteRumor != nil {
		g.l.Info("got client route rumor message")
		if g.isSimpleMode {
			g.l.Warn("simple mode should not do route rumoring!")
			return
		}
		messageId := g.messageStorage.GetNextMessageId(gossiperName)
		rmsg := &RumorMessage{OriginalName: gossiperName, ID: messageId, Text: ""} // distributing message with empty text
		g.processRumorMessage(rmsg)
	} else if cmsg.Private != nil {
		g.l.Info("got client private message")
		pmsg := &PrivateMessage{Origin: gossiperName, ID: 0, Text: cmsg.Private.Text, Destination: cmsg.Private.Destination, HopLimit: DefaultHopLimit}
		g.processPrivateMessage(pmsg)
	} else if cmsg.ToShare != nil {
		g.l.Info("got client to share message")
		g.processClientToShare(cmsg.ToShare)
	} else if cmsg.ToDownload != nil {
		g.l.Info("got client to download message")
		g.processClientDataRequest(cmsg.ToDownload)
	} else if cmsg.ToSearch != nil {
		g.l.Info("got client to search message")
		g.processClientSearchRequest(cmsg.ToSearch)
	}
}

func (g *Gossiper) processAddressedGossipPacket(agp *AddressedGossipPacket) {
	gp := agp.Packet
	address := agp.Address

	g.UpdatePeersIfNeeded(address)

	if gp.Rumor != nil {
		g.l.Info("got rumor-msg " + gp.Rumor.String() + " from " + address.String())
		g.processAddressedRumorMessage(gp.Rumor, address)
	} else if gp.Status != nil {
		g.l.Info("got status from " + address.String())
		g.processAddressedStatusPacket(gp.Status, address)
	} else if gp.Simple != nil {
		g.l.Info("got simple from " + address.String())
		g.processAddressedSimpleMessage(gp.Simple, address)
	} else if gp.Private != nil {
		g.l.Info("got private from " + address.String())
		g.processAddressedPrivateMessage(gp.Private, address)
	} else if gp.DataRequest != nil {
		g.l.Info("got data request message")
		g.processAddressedDataRequest(gp.DataRequest, address)
	} else if gp.DataReply != nil {
		g.l.Info("got data reply message")
		g.processAddressedDataReply(gp.DataReply, address)
	} else if gp.SearchRequest != nil {
		g.l.Info("got search request message")
		g.processAddressedSearchRequest(gp.SearchRequest, address)
	} else if gp.SearchReply != nil {
		g.l.Info("got search reply message")
		g.processAddressedSearchReply(gp.SearchReply, address)
	} else if gp.TxPublish != nil {
		g.l.Info("got tx publish message")
		g.processTxPublish(gp.TxPublish)
	} else if gp.BlockPublish != nil {
		g.l.Info("got block publish message, block: " + gp.BlockPublish.Block.String())
		g.processBlockPublish(gp.BlockPublish)
	}
}

func (g *Gossiper) processAddressedSimpleMessage(smsg *SimpleMessage, address *UDPAddr) {
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()

	smsg.RelayPeerAddr = g.peersConnection.LocalAddr().String()

	for _, peer := range g.peers {
		if address != nil && peer.String() == address.String() {
			continue
		}
		g.l.Info("sending simple to " + peer.String())
		peerMessagesToSend <- &AddressedGossipPacket{Address: peer, Packet: &GossipPacket{Simple: smsg}}
	}
}

func (g *Gossiper) processAddressedStatusPacket(sp *StatusPacket, address *UDPAddr) {
	statusesChannelsMux.Lock()
	if val, isPresent := statusesChannels[address.String()]; isPresent {
		g.l.Info("status from " + address.String() + " found in map, forwarding it to corresponding goroutine...")
		val <- sp
		delete(statusesChannels, address.String()) // goroutine cannot eat more, than one status packet, so delete it from map
		statusesChannelsMux.Unlock()
		return
	}
	statusesChannelsMux.Unlock()

	g.l.Info("got status not from map, interesting")
	rmsg, otherHasSomethingNew := g.messageStorage.Diff(sp)
	if rmsg != nil {
		g.spreadTheRumor(rmsg, address)
	} else if otherHasSomethingNew {
		peerMessagesToSend <- &AddressedGossipPacket{Packet: &GossipPacket{Status: g.messageStorage.GetCurrentStatusPacket()}, Address: address}
	} else {
		g.l.Info("nothing interesting in the status")
		fmt.Println("IN SYNC WITH " + address.String())
	}
	// else do nothing at all
}

func (g *Gossiper) processAddressedRumorMessage(rmsg *RumorMessage, address *UDPAddr) {

	// send status back to rumorer:
	feedbackStatus := &GossipPacket{Status: g.messageStorage.GetCurrentStatusPacket()}
	addressedFeedbackStatus := &AddressedGossipPacket{Address: address, Packet: feedbackStatus}
	g.l.Info("sending status as feedback to " + address.String())
	peerMessagesToSend <- addressedFeedbackStatus

	if g.messageStorage.IsNewMessage(rmsg) {
		// update next hop data
		g.updateNextHop(rmsg.OriginalName, address)
	}

	g.processRumorMessage(rmsg)
}

func (g *Gossiper) processRumorMessage(rmsg *RumorMessage) {
	isNewMessage := g.messageStorage.AddRumorMessage(rmsg)

	if isNewMessage {
		// rumormongering -- choose random peer to send rmsg to
		// can do optimization of not sending rumour to its sender, but it's not that necessary
		if g.arePeersEmpty() {
			g.l.Warn("no peers are known, cannot do rumor-mongering")
			return // msg came from client and no peers are known
		}

		g.spreadTheRumor(rmsg, nil)
	} else {
		// if we got an old rumor, then let's do nothing, we already have sent a feedback, everything is good
		g.l.Info("message is not new, skipping it")
	}
}

func (g *Gossiper) processAddressedPrivateMessage(pmsg *PrivateMessage, address *UDPAddr) {
	//  commented in order to have all announcements from rumor message and to pass tests, may uncomment, really not that important
	//g.updateNextHop(pmsg.Origin, address)
	g.processPrivateMessage(pmsg)
}

// in functions below copy-paste is present, but moving it to abstract functions is too resource-taking
func (g *Gossiper) processPrivateMessage(pmsg *PrivateMessage) {
	gossiperName := g.name.Load().(string)

	if pmsg.Destination == gossiperName {
		g.messageStorage.AddPrivateMessage(pmsg, gossiperName)
		return
	}
	if pmsg.HopLimit <= 0 {
		g.l.Warn("hop limit for forwarding exceeded, drop the msg..")
		return
	}

	pmsg.HopLimit = pmsg.HopLimit - 1
	g.sendPacketWithNextHop(pmsg.Destination, &GossipPacket{Private: pmsg})
}

func (g *Gossiper) processAddressedDataRequest(drqmsg *DataRequest, address *UDPAddr) {
	//g.updateNextHop(drqmsg.Origin, address)
	g.processDataRequest(drqmsg)
}

// gossiper answers with either (shared file data) or (download(ing|ed) file data)
func (g *Gossiper) processDataRequest(drqmsg *DataRequest) {
	gossiperName := g.name.Load().(string)

	if drqmsg.Destination == gossiperName {
		requestedData := g.sharedFilesManager.GetChunkOrMetafile(drqmsg.HashValue)
		if requestedData == nil {
			requestedData = g.downloadingFilesManager.GetChunkOrMetafile(drqmsg.HashValue) // look not only in shared, but also in downloaded
			if requestedData == nil {
				g.l.Error("requested unexisting chunk: requested by " + drqmsg.Origin)
				return
			}
		}

		g.l.Info("answered to data request from " + drqmsg.Origin + " with chunk/metafile")
		drpmsg := &DataReply{HashValue: drqmsg.HashValue, Origin: gossiperName, Destination: drqmsg.Origin, HopLimit: DefaultHopLimit, Data: requestedData}
		g.sendPacketWithNextHop(drpmsg.Destination, &GossipPacket{DataReply: drpmsg})
		return
	}

	if drqmsg.HopLimit <= 0 {
		g.l.Warn("hop limit for forwarding exceeded, drop the msg..")
		return
	}

	drqmsg.HopLimit = drqmsg.HopLimit - 1
	g.sendPacketWithNextHop(drqmsg.Destination, &GossipPacket{DataRequest: drqmsg})
}

func (g *Gossiper) processAddressedDataReply(drpmsg *DataReply, address *UDPAddr) {
	//g.updateNextHop(drpmsg.Origin, address)
	g.processDataReply(drpmsg)
}

func (g *Gossiper) processDataReply(drpmsg *DataReply) {
	gossiperName := g.name.Load().(string)

	if drpmsg.Destination != gossiperName {
		if drpmsg.HopLimit <= 0 {
			g.l.Warn("hop limit for forwarding exceeded, drop the drpmsg..")
			return
		}

		drpmsg.HopLimit = drpmsg.HopLimit - 1
		g.sendPacketWithNextHop(drpmsg.Destination, &GossipPacket{DataReply: drpmsg})
		return
	}

	// else msg addressed to this gossiper:
	downloadingFilesChannelsMux.Lock()
	defer downloadingFilesChannelsMux.Unlock()

	if ch, ok := downloadingFilesChannels[drpmsg.Origin]; ok {
		ch <- drpmsg
	} else {
		g.l.Warn("we are downloading nothing from this host! (the host is not present in the map)")
	}
}

func (g *Gossiper) processAddressedSearchRequest(srqmsg *SearchRequest, address *UDPAddr) {
	// check, that we haven't answered same search request not so long ago
	g.recentSearchRequestsMux.Lock()
	setkey := GetRecentSearchRequestSetKey(srqmsg.Origin, srqmsg.Keywords)
	_, recentlyAnswered := g.recentSearchRequests[setkey]
	if recentlyAnswered {
		g.l.Warn("this search request was already recently answered, dropping it...")
		g.recentSearchRequestsMux.Unlock()
		return
	}

	// else start processing & schedule removing from map
	g.recentSearchRequests[setkey] = true
	go func() {
		// search-request-timeout function
		time.Sleep(RecentSearchRequestTimeout)
		g.recentSearchRequestsMux.Lock()
		delete(g.recentSearchRequests, setkey)
		g.recentSearchRequestsMux.Unlock()
	}()
	g.recentSearchRequestsMux.Unlock()

	//g.updateNextHop(srqmsg.Origin, address)
	g.processSearchRequest(srqmsg)
}

func (g *Gossiper) processAddressedSearchReply(srpmsg *SearchReply, address *UDPAddr) {
	gossiperName := g.name.Load().(string)

	if srpmsg.Destination != gossiperName {
		if srpmsg.HopLimit <= 0 {
			g.l.Warn("hop limit for forwarding exceeded, drop the srpmsg..")
			return
		}

		srpmsg.HopLimit = srpmsg.HopLimit - 1
		g.sendPacketWithNextHop(srpmsg.Destination, &GossipPacket{SearchReply: srpmsg})
		return
	}

	// else msg addressed to this gossiper:
	if g.currentSearchRequest == nil || !g.currentSearchRequest.IsAlive() {
		g.l.Warn("got search reply message, but current-search-request has either not been initiated, or was already finished, dropping the message...")
		return
	}

	g.currentSearchRequest.ForwardSearchReply(srpmsg)
}

func (g *Gossiper) processTxPublish(tx *TxPublish) {
	isNew := g.blockchainManager.AddTransaction(tx)
	if isNew {
		if tx.HopLimit <= 0 {
			g.l.Info("dropping tx publish, hoplimit reached 0")
			return
		}
		g.sendAllPeers(&GossipPacket{TxPublish: &TxPublish{HopLimit: tx.HopLimit - 1, File: tx.File}})
	}
}

func (g *Gossiper) processBlockPublish(bp *BlockPublish) {
	isNew := g.blockchainManager.AddBlock(&bp.Block)
	if isNew {
		if bp.HopLimit <= 0 {
			g.l.Info("dropping block publish, hoplimit reached 0")
			return
		}
		g.sendAllPeers(&GossipPacket{BlockPublish: &BlockPublish{HopLimit: bp.HopLimit - 1, Block: bp.Block}})
	}
}

func (g *Gossiper) processClientToShare(ctsmsg *ClientToShareMessage) {
	name, metafileHash, size := g.sharedFilesManager.ShareFile(ctsmsg.Path)

	if name == nil || metafileHash == nil || size == nil {
		g.l.Warn("while sharing an error occured, thus tx not formed")
		return
	}

	f := File{Name: *name, MetafileHash: (*metafileHash)[:], Size: int64(*size)}
	tx := &TxPublish{File: f, HopLimit: BlockchainTxHopLimit}

	isAllowed := g.blockchainManager.AddTransaction(tx)
	if !isAllowed {
		g.l.Warn("file shared, but name cannot be reserved, because it's already reserved in blockchain")
		return
	}

	g.l.Info("new transaction from file-sharing added, gossiping the transaction to peers")
	g.sendAllPeers(&GossipPacket{TxPublish: tx})
}

func (g *Gossiper) processClientDataRequest(cdrqmsg *ClientToDownloadMessage) {
	origin := cdrqmsg.Destination

	if cdrqmsg.Destination == "" {
		g.l.Info("got client to-download request with no dest specified, so checking for such hash in files, downloaded during previous search-request..")
		g.currentSearchRequest.DownloadFileByHash(cdrqmsg.HashValue, cdrqmsg.Name)
		return
	}

	downloadingFilesChannelsMux.Lock() // if locking later, rc is introduced
	if !g.downloadingFilesManager.StartDownloadingFromOrigin(origin, cdrqmsg.Name, cdrqmsg.HashValue) {
		g.l.Error("cannot start downloading, because downloading from this peer is already in progress")
		downloadingFilesChannelsMux.Unlock()
		return
	}

	if _, ok := downloadingFilesChannels[origin]; ok {
		g.l.Error("whaaat, map is not clean!") // we trust dfm more
	}
	downloadingFilesChannels[origin] = make(chan *DataReply)
	downloadingFilesChannelsMux.Unlock()

	// send first data request and start file-downloading goroutine
	g.l.Info("starting file-downloading goroutine & requesting metafile from " + origin + " for file " + cdrqmsg.Name)
	drqmsg := &DataRequest{Destination: origin, HopLimit: DefaultHopLimit, HashValue: cdrqmsg.HashValue[:], Origin: g.name.Load().(string)}
	gp := &GossipPacket{DataRequest: drqmsg}
	g.sendPacketWithNextHop(origin, gp)

	go g.startFileDownloadingGoroutine(origin, cdrqmsg.HashValue[:])
}

func (g *Gossiper) processClientSearchRequest(csrqmsg *ClientToSearchMessage) {
	if g.currentSearchRequest != nil && g.currentSearchRequest.IsAlive() {
		g.l.Error("cannot start new search request, until previous haven't finished, retry your query later pls...")
		return
	}

	g.currentSearchRequest = nil // feed gc with new victim

	ch := make(chan *SearchReply)
	g.currentSearchRequest = InitCurrentSearchRequest(ch, g.clientAddress.Port, g.l)

	go g.startFileSearchingGoroutine(csrqmsg.Keywords, int(csrqmsg.Budget), ch)
}

// -------------------------------------------
// end of message-processor thread section
// -------------------------------------------

// called both by message-processor and rumor-mongering threads
// if peer is nil, choose random peer
func (g *Gossiper) spreadTheRumor(rmsg *RumorMessage, peer *UDPAddr) {

	if peer == nil {
		peer = g.getRandomPeer()
	}

	statusesChannelsMux.Lock()
	if _, contains := statusesChannels[peer.String()]; contains {
		g.l.Warn("rumormongering with this peer is already in progress, this case is too hard for me")
		statusesChannelsMux.Unlock()
		return
	}
	ch := make(chan *StatusPacket)
	statusesChannels[peer.String()] = ch
	statusesChannelsMux.Unlock()

	g.l.Info("rumor sent further to " + peer.String())
	peerMessagesToSend <- &AddressedGossipPacket{Address: peer, Packet: &GossipPacket{Rumor: rmsg}}
	fmt.Println("MONGERING with " + peer.String())

	go g.startRumorMongeringThread(rmsg, ch, peer)
}

// called both by message-processor & search-request goroutine
func (g *Gossiper) processSearchRequest(srqmsg *SearchRequest) {
	gossiperName := g.name.Load().(string)

	// if got message not from this gossiper, scan its shared&downloaded files for what they ask for
	budget := int(srqmsg.Budget)
	if srqmsg.Origin != gossiperName {
		sharedMatchedFiles := g.sharedFilesManager.GetSearchResults(srqmsg.Keywords)
		if len(sharedMatchedFiles) > 0 {
			g.l.Info("got " + strconv.Itoa(len(sharedMatchedFiles)) + " shared matched files for search request " + strings.Join(srqmsg.Keywords, ",") + " from " + srqmsg.Origin)
		}
		downloadedMatchedFiles := g.downloadingFilesManager.GetSearchResults(srqmsg.Keywords)
		if len(downloadedMatchedFiles) > 0 {
			g.l.Info("got " + strconv.Itoa(len(downloadedMatchedFiles)) + " download(ed|ing) matched files for search request " + strings.Join(srqmsg.Keywords, ",") + " from " + srqmsg.Origin)
		}

		matchedFiles := append(sharedMatchedFiles, downloadedMatchedFiles...)
		if len(matchedFiles) > 0 {
			g.l.Info("got some matches on " + gossiperName + " for search-request: " + strings.Join(srqmsg.Keywords, ","))
			gp := &GossipPacket{SearchReply: &SearchReply{Origin: gossiperName, Destination: srqmsg.Origin, HopLimit: DefaultHopLimit, Results: matchedFiles}}
			g.sendPacketWithNextHop(srqmsg.Origin, gp)
		} else {
			g.l.Info("got no matches on " + gossiperName + " for search-request: " + strings.Join(srqmsg.Keywords, ","))
		}

		budget = budget - 1
		if budget <= 0 {
			g.l.Info("budget on " + strings.Join(srqmsg.Keywords, ",") + " became zero, dropping it, sry")
			return
		}
	}

	// send search request further:
	g.peersSliceMux.Lock()
	defer g.peersSliceMux.Unlock()
	residual := budget % len(g.peers)
	for _, peer := range g.peers {
		newbudget := budget / len(g.peers)
		if residual > 0 {
			newbudget++
			residual--
		}

		agp := &AddressedGossipPacket{Address: peer, Packet: &GossipPacket{SearchRequest: &SearchRequest{Budget: uint64(newbudget), Keywords: srqmsg.Keywords, Origin: srqmsg.Origin}}}
		peerMessagesToSend <- agp
	}
}

// --------------------------------------
// rumor-mongering thread section:
// --------------------------------------

func (g *Gossiper) startRumorMongeringThread(messageBeingRumored *RumorMessage, ch chan *StatusPacket, peer *UDPAddr) {
	ticker := time.NewTicker(RumorTimeout)
	select {
	case <-ticker.C:
		g.l.Info("peer " + peer.String() + " exceeded the timeout")

		// clean the map
		statusesChannelsMux.Lock()
		delete(statusesChannels, peer.String())
		statusesChannelsMux.Unlock()

		g.flipRumorMongeringCoin(messageBeingRumored)
	case statusPacket := <-ch:
		g.l.Info("processing status-response in rumor-mongering thread from " + peer.String())

		rmsg, otherHasSomethingNew := g.messageStorage.Diff(statusPacket)
		if rmsg != nil {
			g.l.Info("peer " + peer.String() + " doesn't know rmsg, sending it: " + rmsg.String())
			g.spreadTheRumor(rmsg, peer)
		} else if otherHasSomethingNew {
			g.l.Info("peer " + peer.String() + " knows more, than me, sending status to him")
			agp := &AddressedGossipPacket{Packet: &GossipPacket{Status: g.messageStorage.GetCurrentStatusPacket()}, Address: peer}
			peerMessagesToSend <- agp
		} else {
			fmt.Println("IN SYNC WITH " + peer.String())
			g.l.Info("peer " + peer.String() + " has same info as me, flipping the coin")
			g.flipRumorMongeringCoin(messageBeingRumored)
		}
	}
	// let the goroutine die!
}

func (g *Gossiper) flipRumorMongeringCoin(messageBeingRumored *RumorMessage) {
	continueMongering := (rand.Int() % 2) == 0
	if continueMongering {
		peer := g.getRandomPeer() // crutch because of fmt. requirements in HW1 :(
		g.l.Info("coin says to continue rumor-mongering")
		fmt.Println("FLIPPED COIN sending rumor to " + peer.String())
		g.spreadTheRumor(messageBeingRumored, peer)
	} else {
		g.l.Info("coin says to stop rumor-mongering of " + messageBeingRumored.String())
	}
}

// --------------------------------------
// end of rumor-mongering thread section
// --------------------------------------

// called only by file-downloading goroutines:
func (g *Gossiper) startFileDownloadingGoroutine(origin string, latestRequestedHash []byte) {
	downloadingFilesChannelsMux.Lock()
	ch, ok := downloadingFilesChannels[origin]
	if !ok {
		g.l.Error("some terrible race condition occured, channel is not in the map! Downloading goroutine dies..")
		return
	}
	downloadingFilesChannelsMux.Unlock()

	ticker := time.NewTicker(FileDownloadReplyTimeout)
	timeoutsLimit := FileDownloadTimeoutsLimit

	for {
		select {
		case <-ticker.C:
			g.l.Debug("timeout in file-downloading shot")
			timeoutsLimit = timeoutsLimit - 1
			if timeoutsLimit <= 0 {
				g.l.Error("timeout completely exceeded for receiving the file")

				downloadingFilesChannelsMux.Lock()
				delete(downloadingFilesChannels, origin)
				g.downloadingFilesManager.DropDownloading(origin)
				downloadingFilesChannelsMux.Unlock()
				return
			}

			// resend message we didn't receive reply for
			g.l.Info("resending msg because of timeout: DataRequest with hash: " + hex.EncodeToString(latestRequestedHash))
			dataRequest := &DataRequest{Destination: origin, HopLimit: DefaultHopLimit, HashValue: latestRequestedHash, Origin: g.name.Load().(string)}
			gp := &GossipPacket{DataRequest: dataRequest}
			g.sendPacketWithNextHop(origin, gp)
		case dataReplyPacket := <-ch:
			g.l.Debug("got chunk/metafile from " + dataReplyPacket.Origin)
			downloadingFilesChannelsMux.Lock() // locking to do removing from map & dfm synchronicaly
			isFinished := g.downloadingFilesManager.ProcessDataReply(origin, dataReplyPacket)
			if isFinished == nil {
				g.l.Error("an error occured when downloading from the peer, try to request the same chunk/metafile once again, hash is: " + hex.EncodeToString(latestRequestedHash))
				dataRequest := &DataRequest{Destination: origin, HopLimit: DefaultHopLimit, HashValue: latestRequestedHash, Origin: g.name.Load().(string)}
				gp := &GossipPacket{DataRequest: dataRequest}
				g.sendPacketWithNextHop(origin, gp)
				downloadingFilesChannelsMux.Unlock()
				break
			}

			if *isFinished {
				g.l.Info("Great, downloading is finished from " + origin)
				delete(downloadingFilesChannels, origin)
				downloadingFilesChannelsMux.Unlock()
				return
			}
			downloadingFilesChannelsMux.Unlock()

			// if not finished, send new message and continue waiting..
			ticker = time.NewTicker(FileDownloadReplyTimeout) // upd ticker not to shoot to early
			timeoutsLimit = FileDownloadTimeoutsLimit

			dataRequestHash := g.downloadingFilesManager.GetDataRequestHash(origin)
			if dataRequestHash == nil {
				g.l.Error("cannot get a new chunk hash to download..")
				break // wait for new msgs to come maybe
			}

			g.l.Debug("now requesting " + hex.EncodeToString(dataRequestHash))
			latestRequestedHash = dataRequestHash
			dataRequest := &DataRequest{Destination: origin, HopLimit: DefaultHopLimit, HashValue: dataRequestHash, Origin: g.name.Load().(string)}
			gp := &GossipPacket{DataRequest: dataRequest}
			g.sendPacketWithNextHop(origin, gp)
		}
	}
}

// called only by the only file-searching goroutine:
func (g *Gossiper) startFileSearchingGoroutine(keywords []string, budget int, ch chan *SearchReply) {
	maxAllowedBudget := FileSearchMaxBudget
	if budget == 0 {
		budget = FileSearchStartBudget
	} else {
		// budget is specified explicitly, so small hack to make the request only once
		maxAllowedBudget = budget
	}

	ticker := time.NewTicker(1)

	for {
		select {
		case <-ticker.C:
			if budget > maxAllowedBudget {
				if budget > maxAllowedBudget+1 {
					g.l.Warn("reached max budget & didn't get enough full matches, leaving the function, defeated...")
					g.currentSearchRequest.Shutdown()
					return
				}

				g.l.Info("reached maximum budget, stopped sending search-requests, just wait a bit more for more answers to arrive...")
				ticker = time.NewTicker(FileSearchReplyTimeout * 2)
				budget++
				continue
			}

			// send search request:
			g.l.Info("sending search request for keywords: " + strings.Join(keywords, ",") + " for budget: " + strconv.Itoa(budget))
			searchRequest := &SearchRequest{Budget: uint64(budget), Keywords: keywords, Origin: g.name.Load().(string)}
			g.processSearchRequest(searchRequest)

			budget++
			ticker = time.NewTicker(FileSearchReplyTimeout)
		case dataReplyPacket := <-ch:
			for _, res := range dataReplyPacket.Results {
				// here a great question about task formulation comes. I consider only full matches significant.
				// we do search until we get threshold of full matches, where full match is a gossiper, which "has all the chunks"
				// then we can start downloading from this peer. I believe, that it is ok with task we're required to implement
				if uint64(len(res.ChunkMap)) == res.ChunkCount {
					// this is a full match
					metahash, err := GetTypeStrictHash(res.MetafileHash)
					if err != nil {
						g.l.Warn("cannot convert metafile hash...")
						continue
					}

					fullMatch := FullSearchMatch{Origin: dataReplyPacket.Origin, Filename: res.FileName, MetafileHash: metahash}
					g.currentSearchRequest.AddFullMatch(&fullMatch)
					g.l.Debug("got a full match from " + dataReplyPacket.Origin + " on " + res.FileName + ", has " + strconv.Itoa(g.currentSearchRequest.GetFullMatchesNumber()) + " matches now")
				} else {
					g.l.Debug("got a partial match from " + dataReplyPacket.Origin + " on " + res.FileName + ", skipping it, see code for comment on why so is done")
				}

				// also do pretty-print:
				fmt.Println("FOUND match " + res.FileName + " at " + dataReplyPacket.Origin +
					" metafile=" + hex.EncodeToString(res.MetafileHash) +
					" chunks=" + strings.Trim(strings.Join(strings.Fields(fmt.Sprint(res.ChunkMap)), ","), "[]"))
			}

			if g.currentSearchRequest.GetFullMatchesNumber() >= FileSearchFullMatchesThreshold {
				g.l.Info("reached threshold for search request: " + strings.Join(keywords, ",") + ", stopping the search-request goroutine")
				//g.l.Info("initiating download process..")
				//g.currentSearchRequest.DownloadAnyFile(g.name.Load().(string))
				fmt.Println("SEARCH FINISHED")
				g.currentSearchRequest.Shutdown()
				return
			}
		}
	}
}
