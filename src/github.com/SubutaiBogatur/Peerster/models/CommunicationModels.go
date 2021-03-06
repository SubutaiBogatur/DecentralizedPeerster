package models

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	. "github.com/SubutaiBogatur/Peerster/config"
	"net"
	"strconv"
)

type AddressedGossipPacket struct {
	Packet  *GossipPacket
	Address *net.UDPAddr
}

// the invariant on the packet is that only one of the fields is not nil
type GossipPacket struct {
	Simple        *SimpleMessage
	Rumor         *RumorMessage
	Status        *StatusPacket
	Private       *PrivateMessage
	DataRequest   *DataRequest
	DataReply     *DataReply
	SearchRequest *SearchRequest
	SearchReply   *SearchReply
	TxPublish     *TxPublish
	BlockPublish  *BlockPublish
}

type SimpleMessage struct {
	OriginalName  string // name of original gossiper sender
	RelayPeerAddr string // address of latest peer retranslator in the form ip:port
	Text          string
}

type RumorMessage struct {
	OriginalName string // name of original gossiper sender
	ID           uint32 // id assigned by original sender ie counter per sender
	Text         string
}

type StatusPacket struct {
	Want []PeerStatus // vector clock
}

// depicts gossiper's information about another peer
type PeerStatus struct {
	Identifier string
	NextID     uint32
}

type PrivateMessage struct {
	Origin      string
	ID          uint32
	Text        string
	Destination string
	HopLimit    uint32
}

type DataRequest struct {
	Origin      string
	Destination string
	HopLimit    uint32
	HashValue   []byte
}

type DataReply struct {
	Origin      string
	Destination string
	HopLimit    uint32
	HashValue   []byte
	Data        []byte
}

type SearchRequest struct {
	Origin   string
	Budget   uint64
	Keywords []string
}

type SearchReply struct {
	Origin      string
	Destination string
	HopLimit    uint32
	Results     []*SearchResult
}

type SearchResult struct {
	FileName     string
	MetafileHash []byte
	ChunkMap     []uint64
	ChunkCount   uint64
}

type TxPublish struct {
	File     File
	HopLimit uint32
}

type BlockPublish struct {
	Block    Block
	HopLimit uint32
}

type File struct {
	Name         string
	Size         int64
	MetafileHash []byte
}

type Block struct {
	PrevHash     [32]byte
	Nonce        [32]byte
	Transactions []TxPublish
}

type ClientMessage struct {
	Rumor      *ClientRumorMessage
	RouteRumor *ClientRouteRumorMessage
	Private    *ClientPrivateMessage
	ToShare    *ClientToShareMessage
	ToDownload *ClientToDownloadMessage
	ToSearch   *ClientToSearchMessage
}

type ClientRumorMessage struct {
	Text string
}

type ClientRouteRumorMessage struct{}

type ClientPrivateMessage struct {
	Text        string
	Destination string
}

type ClientToShareMessage struct {
	Path string // path to file relative to _SharedFiles folder
}

type ClientToDownloadMessage struct {
	Name        string // name to give to file after downloading finishes
	Destination string
	HashValue   [32]byte
}

type ClientToSearchMessage struct {
	Keywords []string
	Budget   uint64 // 0 = no budget provided
}

func (rmsg *RumorMessage) String() string {
	return rmsg.OriginalName + ":" + strconv.Itoa(int(rmsg.ID))
}

func (cmsg *ClientMessage) Print() bool {
	if cmsg.Rumor != nil {
		rcmsg := cmsg.Rumor
		fmt.Println("CLIENT MESSAGE " + rcmsg.Text)
	} else if cmsg.Private != nil {
		pcmsg := cmsg.Private
		fmt.Println("CLIENT PRIVATE TO " + pcmsg.Destination + ": " + pcmsg.Text)
	} else if cmsg.ToShare != nil {
		tscmsg := cmsg.ToShare
		fmt.Println("CLIENT SHARE REQUEST: " + tscmsg.Path)
	} else if cmsg.ToDownload != nil {
		tdcmsg := cmsg.ToDownload
		fmt.Println("CLIENT DOWNLOAD REQUEST: " + tdcmsg.Name + " from " + tdcmsg.Destination + " hash " + hex.EncodeToString(tdcmsg.HashValue[:]))
	} else {
		// client route rumor message
		return false
	}
	return true
}

func (agp *AddressedGossipPacket) Print() bool {
	gp := agp.Packet

	if gp.Rumor != nil {
		if gp.Rumor.Text == "" {
			return false
		}
		rmsg := gp.Rumor
		fmt.Println("RUMOR origin " + rmsg.OriginalName + " from " + agp.Address.String() + " ID " + strconv.Itoa(int(rmsg.ID)) + " contents " + rmsg.Text)
	} else if gp.Status != nil {
		status := gp.Status
		fmt.Print("STATUS from " + agp.Address.String())
		for _, peerStatus := range status.Want {
			fmt.Print(" peer " + peerStatus.Identifier + " nextID " + strconv.Itoa(int(peerStatus.NextID)))
		}
		fmt.Println()
	} else if gp.Simple != nil {
		smsg := gp.Simple
		fmt.Println("SIMPLE MESSAGE origin " + smsg.OriginalName + " from " + smsg.RelayPeerAddr + " contents " + smsg.Text)
	} else if gp.Private != nil {
		pmsg := gp.Private
		fmt.Println("PRIVATE origin " + pmsg.Origin + " hop-limit " + fmt.Sprint(pmsg.HopLimit) + " contents " + pmsg.Text)
	} else if gp.DataReply != nil {
		//drpmsg := gp.DataReply
		//fmt.Println("REPLY origin " + drpmsg.Origin + " hash " + hex.EncodeToString(drpmsg.HashValue[:]))
		return false
	} else if gp.DataRequest != nil {
		//drqmsg := gp.DataRequest
		//fmt.Println("REQUEST destination " + drqmsg.Destination + " hash " + hex.EncodeToString(drqmsg.HashValue[:]))
		return false
	} else {
		return false // never should happen
	}
	return true
}

func (t *TxPublish) Hash() (out [32]byte) {
	h := sha256.New()
	binary.Write(h, binary.LittleEndian, uint32(len(t.File.Name)))
	h.Write([]byte(t.File.Name))
	h.Write(t.File.MetafileHash)
	copy(out[:], h.Sum(nil))
	return
}

func (b *Block) Hash() (out [32]byte) {
	h := sha256.New()
	h.Write(b.PrevHash[:])
	h.Write(b.Nonce[:])
	binary.Write(h, binary.LittleEndian, uint32(len(b.Transactions)))
	for _, t := range b.Transactions {
		th := t.Hash()
		h.Write(th[:])
	}
	copy(out[:], h.Sum(nil))
	return
}

func (b *Block) IsGood() bool {
	hash := b.Hash()
	for i := 0; i < BlockhainBytesForGoodBlock; i++ {
		if hash[i] != 0 {
			return false
		}
	}

	return true
}

func (b *Block) String() string {
	h := b.Hash()
	return hex.EncodeToString(h[:])
}
