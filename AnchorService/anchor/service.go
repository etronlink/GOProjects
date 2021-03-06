package anchor

import (
	"AnchorService/common"
	"AnchorService/util"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FactomProject/factom"
	"github.com/FactomProject/factomd/anchor"
	"github.com/FactomProject/factomd/common/primitives"
	"io/ioutil"
	"net/http"
	"os"
	"syscall"
	"time"
)

var log = util.AnchorLogger

type Anchor interface {
	PlaceAnchor(msg common.DirectoryBlockAnchorInfo)
}

type AnchorService struct {
	DoAnchor      Anchor
	serverECKey   *factom.ECAddress
	sigKey        *primitives.PrivateKey
	factomserver  string
	anchorChainID *common.Hash
	DirBlockMsg   chan common.DirectoryBlockAnchorInfo
	AnchorFail    chan bool
}

func NewAnchorService(DirBlockMsg chan common.DirectoryBlockAnchorInfo, AnchorFail chan bool) *AnchorService {
	cfg := util.ReadConfig()
	service := new(AnchorService)
	var err error

	service.serverECKey, err = factom.GetECAddress(cfg.Anchor.ServerECKey)
	if err != nil {
		panic("Cannot parse Server EC Key from configuration file: " + err.Error())
	}

	service.sigKey, err = primitives.NewPrivateKeyFromHex(cfg.Anchor.SigKey)
	if err != nil {
		panic("Cannot parse signature key Key from configuration file: " + err.Error())
	}
	anchorChainID, err := common.HexToHash(cfg.Anchor.AnchorChainID)

	if err != nil || anchorChainID == nil {
		panic("Cannot parse Server AnchorChainID from configuration file: " + err.Error())
	}

	service.factomserver = cfg.App.FactomAddr
	log.Info("FactomAddress ", "server addr", service.factomserver)

	service.anchorChainID = anchorChainID
	service.DirBlockMsg = DirBlockMsg
	service.AnchorFail = AnchorFail

	if cfg.App.AnchorTo == 0 {
		log.Info("anchor to btc")
		btc := NewAnchorBTC()
		err := btc.InitRPCClient()
		if err != nil {
			log.Crit("Error on init RPC :", "error", err)
		}

		service.DoAnchor = btc
		btc.service = service
		return service
	} else if cfg.App.AnchorTo == 1 {
		log.Info("anchor to eth")
		eth := NewAnchorETH()
		eth.InitEthClient()
		service.DoAnchor = eth
		eth.service = service

		return service
	} else {
		log.Crit("Not support this kind of anchor, check your configuration file")
	}

	return nil
}

func (service *AnchorService) Start() {
	log.Info("Start Anchor service...")

	failedTime := 0
	for {
		select {
		case anchorMsg := <-service.DirBlockMsg:
			log.Info("Got anchor msg: ", "msg", anchorMsg)
			go service.DoAnchor.PlaceAnchor(anchorMsg)
		case _ = <-service.AnchorFail:
			failedTime++
			log.Error("anchor failed", "time", failedTime)
			if failedTime >= 10 {
				log.Error("more than 10 times fail to anchor, just quit job")
				p, _ := os.FindProcess(os.Getpid())
				p.Signal(syscall.SIGQUIT)
			}
		}
	}
}

func NewEntry(chainid string, external, content []byte) *factom.Entry {
	entry := new(factom.Entry)
	entry.ChainID = chainid
	entry.Content = content
	bta := make([][]byte, 0)
	bta = append(bta, external)
	entry.ExtIDs = bta

	return entry
}

func (anchor *AnchorService) submitEntryToAnchorChain(anchorRec *anchor.AnchorRecord) error {
	raw, sign, err := anchorRec.MarshalAndSignV2(anchor.sigKey)
	if err != nil {
		return err
	}

	newentry := NewEntry(anchor.anchorChainID.String(), sign, raw)
	commit, err := factom.ComposeEntryCommit(newentry, anchor.serverECKey)
	if err != nil {
		return err
	}

	commitBody, err := util.EncodeJSON(commit)

	if err != nil {
		log.Error("Encode error ", commitBody)
		return err
	}

	httpClient := http.DefaultClient
	log.Info("do commit ", "commit", string(commitBody))
	re, err := http.NewRequest("POST", fmt.Sprintf("http://%s/v2", anchor.factomserver), bytes.NewBuffer(commitBody))

	if err != nil {
		log.Error("error happened, for entry commit ", err)
		return err
	}

	resp, err := httpClient.Do(re)
	if err != nil {
		log.Error("Error for http request ", err)
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		log.Error("Factomd username/password incorrect.  Edit factomd.conf or\ncall factom-cli with -factomduser=<user> -factomdpassword=<pass>")
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	r := util.NewJSON2Response()
	if err := json.Unmarshal(body, r); err != nil {
		log.Error("Error on http request parse body", err)
	}

	log.Debug("Got response for commit entry ", "entry", r)
	time.Sleep(2000)
	rev, err := factom.ComposeEntryReveal(newentry)
	if err != nil {
		log.Info("Got error on entry compose ", err)
	}

	revBody, err := util.EncodeJSON(rev)

	log.Info("Do reveal ", "body", string(revBody))
	if err != nil {
		log.Error("Encode error ", revBody)
	}

	re, err = http.NewRequest("POST", fmt.Sprintf("http://%s/v2", anchor.factomserver), bytes.NewBuffer(revBody))

	if err != nil {
		log.Error("error happened, for entry revl ", err)
		return err
	}

	resp2, err := httpClient.Do(re)
	if err != nil {
		log.Error("Error for http request ", err)
		return err
	}

	if resp2.StatusCode == http.StatusUnauthorized {
		log.Error("Factomd username/password incorrect.  Edit factomd.conf or\ncall factom-cli with -factomduser=<user> -factomdpassword=<pass>")
	}
	defer resp2.Body.Close()

	body, err = ioutil.ReadAll(resp2.Body)

	r = util.NewJSON2Response()
	if err := json.Unmarshal(body, r); err != nil {
		log.Error("Error on http request parse body", err)
	}

	log.Debug("Got response for reveal", "response", r)
	return nil
}

func PrependBlockHeight(height uint32, hash []byte) ([]byte, error) {
	// dir block genesis block height starts with 0, for now
	// similar to bitcoin genesis block
	h := uint64(height)
	if 0xFFFFFFFFFFFF&h != h {
		return nil, errors.New("bad block height")
	}

	header := []byte{'F', 'a'}
	big := make([]byte, 8)
	binary.BigEndian.PutUint64(big, h) //height)

	newdata := append(big[2:8], hash...)
	newdata = append(header, newdata...)
	return newdata, nil
}
