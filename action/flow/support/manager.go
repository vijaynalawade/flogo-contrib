package support

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"

	"github.com/TIBCOSoftware/flogo-contrib/action/flow/definition"
	"github.com/TIBCOSoftware/flogo-contrib/action/flow/script/fggos"
	"github.com/TIBCOSoftware/flogo-lib/app/resource"
	"github.com/TIBCOSoftware/flogo-lib/logger"
	"github.com/TIBCOSoftware/flogo-lib/util"
)

const (
	uriSchemeFile = "file://"
	uriSchemeHttp = "http://"
	uriSchemeRes  = "res://"
	RESTYPE_FLOW  = "flow"
)

var defaultManager *FlowManager

func GetFlowManager() *FlowManager {
	return defaultManager
}

type FlowManager struct {
	resFlows map[string]*definition.Definition

	//todo switch to cache
	rfMu         sync.Mutex // protects the flow maps
	remoteFlows  map[string]*definition.Definition
	flowProvider definition.Provider
}

func NewFlowManager(flowProvider definition.Provider) *FlowManager {
	manager := &FlowManager{}
	manager.resFlows = make(map[string]*definition.Definition)

	if flowProvider != nil {
		manager.flowProvider = flowProvider
	} else {
		manager.flowProvider = &BasicRemoteFlowProvider{}
	}

	//temp hack
	defaultManager = manager

	return manager
}

func (rm *FlowManager) LoadResource(config *resource.Config) error {

	var flowDefBytes []byte

	if config.Compressed {
		decodedBytes, err := decodeAndUnzip(string(config.Data))
		if err != nil {
			return fmt.Errorf("error decoding compressed resource with id '%s', %s", config.ID, err.Error())
		}

		flowDefBytes = decodedBytes
	} else {
		flowDefBytes = config.Data
	}

	var defRep *definition.DefinitionRep
	err := json.Unmarshal(flowDefBytes, &defRep)
	if err != nil {
		return fmt.Errorf("error marshalling flow resource with id '%s', %s", config.ID, err.Error())
	}

	flow, err := rm.materializeFlow(defRep)
	if err != nil {
		return err
	}

	rm.resFlows[config.ID] = flow
	return nil
}

func (rm *FlowManager) GetResource(id string) interface{} {
	return rm.resFlows[id]
}

func (rm *FlowManager) GetFlow(uri string) (*definition.Definition, error) {

	if strings.HasPrefix(uri, uriSchemeRes) {
		return rm.resFlows[uri[6:]], nil
	}

	rm.rfMu.Lock()
	defer rm.rfMu.Unlock()

	if rm.remoteFlows == nil {
		rm.remoteFlows = make(map[string]*definition.Definition)
	}

	flow, exists := rm.remoteFlows[uri]

	if !exists {

		defRep, err := rm.flowProvider.GetFlow(uri)
		if err != nil {
			return nil, err
		}

		flow, err = rm.materializeFlow(defRep)
		if err != nil {
			return nil, err
		}

		rm.remoteFlows[uri] = flow
	}

	return flow, nil
}

func (rm *FlowManager) materializeFlow(flowRep *definition.DefinitionRep) (*definition.Definition, error) {

	def, err := definition.NewDefinition(flowRep)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling flow: %s", err.Error())
	}

	//todo validate flow

	//todo fix this up
	factory := definition.GetLinkExprManagerFactory()

	if factory == nil {
		factory = &fggos.GosLinkExprManagerFactory{}
	}

	def.SetLinkExprManager(factory.NewLinkExprManager(def))
	//todo init activities

	return def, nil

}

type BasicRemoteFlowProvider struct {
}

func (*BasicRemoteFlowProvider) GetFlow(flowURI string) (*definition.DefinitionRep, error) {

	var flowDefBytes []byte

	if strings.HasPrefix(flowURI, uriSchemeFile) {
		// File URI
		logger.Infof("Loading Local Flow: %s\n", flowURI)
		flowFilePath, _ := util.URLStringToFilePath(flowURI)

		readBytes, err := ioutil.ReadFile(flowFilePath)
		if err != nil {
			readErr := fmt.Errorf("error reading flow with uri '%s', %s", flowURI, err.Error())
			logger.Errorf(readErr.Error())
			return nil, readErr
		}
		if readBytes[0] == 0x1f && readBytes[2] == 0x8b {
			flowDefBytes, err = unzip(readBytes)
			if err != nil {
				decompressErr := fmt.Errorf("error uncompressing flow with uri '%s', %s", flowURI, err.Error())
				logger.Errorf(decompressErr.Error())
				return nil, decompressErr
			}
		} else {
			flowDefBytes = readBytes

		}

	} else {
		// URI
		req, err := http.NewRequest("GET", flowURI, nil)
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			getErr := fmt.Errorf("error getting flow with uri '%s', %s", flowURI, err.Error())
			logger.Errorf(getErr.Error())
			return nil, getErr
		}
		defer resp.Body.Close()

		logger.Infof("response Status:", resp.Status)

		if resp.StatusCode >= 300 {
			//not found
			getErr := fmt.Errorf("error getting flow with uri '%s', status code %d", flowURI, resp.StatusCode)
			logger.Errorf(getErr.Error())
			return nil, getErr
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			readErr := fmt.Errorf("error reading flow response body with uri '%s', %s", flowURI, err.Error())
			logger.Errorf(readErr.Error())
			return nil, readErr
		}

		val := resp.Header.Get("flow-compressed")
		if strings.ToLower(val) == "true" {
			decodedBytes, err := decodeAndUnzip(string(body))
			if err != nil {
				decodeErr := fmt.Errorf("error decoding compressed flow with uri '%s', %s", flowURI, err.Error())
				logger.Errorf(decodeErr.Error())
				return nil, decodeErr
			}
			flowDefBytes = decodedBytes
		} else {
			flowDefBytes = body
		}
	}

	var flow *definition.DefinitionRep
	err := json.Unmarshal(flowDefBytes, &flow)
	if err != nil {
		logger.Errorf(err.Error())
		return nil, fmt.Errorf("error marshalling flow with uri '%s', %s", flowURI, err.Error())
	}

	return flow, nil
}

func decodeAndUnzip(encoded string) ([]byte, error) {

	decoded, _ := base64.StdEncoding.DecodeString(encoded)
	return unzip(decoded)
}

func unzip(compressed []byte) ([]byte, error) {

	buf := bytes.NewBuffer(compressed)
	r, err := gzip.NewReader(buf)
	if err != nil {
		return nil, err
	}
	jsonAsBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return jsonAsBytes, nil
}
