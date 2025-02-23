/*
 * Copyright (c) 2021 The AnJia Authors.
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *     http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/anjia0532/apisix-discovery-syncer/model"
	"github.com/ghodss/yaml"
	go_logger "github.com/phachon/go-logger"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

type ApisixClient struct {
	Client        http.Client
	Config        model.Gateway
	UpstreamIdMap map[string]string
	Logger        *go_logger.Logger
	mutex         sync.Mutex
}

var fetchAllUpstream = "upstreams"

func (apisixClient *ApisixClient) GetServiceAllInstances(upstreamName string) ([]model.Instance, error) {
	apisixClient.mutex.Lock()
	if apisixClient.UpstreamIdMap == nil {
		apisixClient.UpstreamIdMap = make(map[string]string)
	}
	upstreamId, ok := apisixClient.UpstreamIdMap[upstreamName]
	if !ok {
		upstreamId = fetchAllUpstream
	}

	uri := apisixClient.Config.AdminUrl + apisixClient.Config.Prefix + upstreamId
	hc := &http.Client{Timeout: 30 * time.Second}

	req, _ := http.NewRequest("GET", uri, nil)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("X-API-KEY", apisixClient.Config.Config["X-API-KEY"])
	resp, err := hc.Do(req)

	if err != nil {
		apisixClient.Logger.Errorf("fetch apisix upstream error,%s", uri)
		return nil, err
	}

	apisixResp := model.ApisixNodeResp{}
	err = json.NewDecoder(resp.Body).Decode(&apisixResp)
	_, _ = io.Copy(ioutil.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		apisixClient.Logger.Errorf("fetch apisix upstream and decode json error,", uri, err)
		return nil, err
	}

	instances := []model.Instance{}
	if upstreamId != fetchAllUpstream {
		apisixResp.Node.Nodes = append(apisixResp.Node.Nodes, apisixResp.Node)
	}
	for _, node := range apisixResp.Node.Nodes {
		apisixClient.UpstreamIdMap[node.Value.Name] = fmt.Sprintf("%s/%s", fetchAllUpstream, node.Value.Id)
		if upstreamName != node.Value.Name {
			continue
		}
		for host, weight := range node.Value.Nodes {
			ts := strings.Split(host, ":")
			p, _ := strconv.Atoi(ts[1])
			instance := model.Instance{Weight: float32(weight), Ip: ts[0], Port: p}
			instances = append(instances, instance)
		}
	}
	apisixClient.mutex.Unlock()
	apisixClient.Logger.Debugf("fetch apisix upstream:%s,instances:%#v", uri, instances)
	return instances, nil
}

var DefaultApisixUpstreamTemplate = `
{
    "timeout": {
        "connect": 30,
        "send": 30,
        "read": 30
    },
    "name": "{{.Name}}",
    "nodes": {{.Nodes}},
    "type":"roundrobin",
    "desc": "auto sync by https://github.com/anjia0532/discovery-syncer"
}
`

func (apisixClient *ApisixClient) SyncInstances(name string, tpl string, discoveryInstances []model.Instance,
	diffIns []model.Instance) error {
	if len(diffIns) == 0 && len(discoveryInstances) == 0 {
		return nil
	}
	//apisix 不支持变量更新nodes，所以diffIns无用，直接用discoveryInstances即可
	method := "PATCH"
	upstreamId, ok := apisixClient.UpstreamIdMap[name]

	nodes := map[string]float32{}
	for _, instance := range discoveryInstances {
		nodes[fmt.Sprintf("%s:%d", instance.Ip, instance.Port)] = instance.Weight
	}
	nodesJson, err := json.Marshal(nodes)
	var body string
	if !ok {
		method = "PUT"
		upstreamId = fetchAllUpstream + "/" + name
		if len(tpl) == 0 {
			tpl = DefaultApisixUpstreamTemplate
		}
		tmpl, err := template.New("UpstreamTemplate").Parse(tpl)
		if err != nil {
			apisixClient.Logger.Errorf("parse apisix UpstreamTemplate failed,tmpl:%s", tmpl)
			return err
		}
		var buf bytes.Buffer
		data := struct {
			Name  string
			Nodes string
		}{Name: name, Nodes: string(nodesJson)}
		err = tmpl.Execute(&buf, data)
		if err != nil {
			apisixClient.Logger.Errorf("parse apisix UpstreamTemplate failed,tmpl:%s,data:%#v", tmpl, data)
		} else {
			body = buf.String()
		}
	} else {
		upstreamId = upstreamId + "/nodes"
		body = string(nodesJson)
	}

	uri := apisixClient.Config.AdminUrl + apisixClient.Config.Prefix + upstreamId
	hc := &http.Client{Timeout: 30 * time.Second}

	req, _ := http.NewRequest(method, uri, bytes.NewBufferString(body))

	req.Header.Add("Accept", "application/json")
	req.Header.Add("X-API-KEY", apisixClient.Config.Config["X-API-KEY"])
	resp, err := hc.Do(req)

	respRawByte, _ := io.ReadAll(resp.Body)

	apisixClient.Logger.Debugf("update apisix upstream uri:%s,method:%s,body:%s,resp:%s",
		uri, method, body, respRawByte)

	if err != nil {
		apisixClient.Logger.Errorf("update apisix upstream uri:%s,method:%s,body:%s,resp:%s failed",
			uri, method, body, respRawByte)
		return err
	}
	if resp.StatusCode >= 400 {
		apisixClient.Logger.Errorf("update apisix upstream uri:%s,method:%s,body:%s,resp:%s failed",
			uri, method, body, respRawByte)
		return nil
	}
	_, _ = io.Copy(ioutil.Discard, resp.Body)
	_ = resp.Body.Close()
	return err
}

func (apisixClient *ApisixClient) fetchInfoFromApisix(uri string) (model.ApisixResp, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	apisixResp := model.ApisixResp{}
	var plugins []string

	url := apisixClient.Config.AdminUrl + apisixClient.Config.Prefix + uri
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("X-API-KEY", apisixClient.Config.Config["X-API-KEY"])
	resp, err := hc.Do(req)

	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]fetch apisix info error,url:%s,err:%s", url, err.Error())
		return apisixResp, err
	}

	// plugins_list need to convert
	if strings.Contains(uri, "plugins/list") {
		plugins = []string{}
		err = json.NewDecoder(resp.Body).Decode(&plugins)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&apisixResp)
	}
	_ = resp.Body.Close()
	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]decode body to struct error,url,%s,err:%s\n", url, err.Error())
		return apisixResp, err
	}
	apisixResp.Node.Nodes = []map[string]interface{}{}
	if len(plugins) > 0 {
		// plugins/list
		for _, pluginName := range plugins {
			apisixPlugin := map[string]interface{}{"name": pluginName}
			if pluginName == "mqtt-proxy" || "dubbo-proxy" == pluginName {
				apisixPlugin["stream"] = true
			}
			apisixResp.Node.Nodes = append(apisixResp.Node.Nodes, apisixPlugin)
		}
	} else {
		// get resp.node.nodes[].value
		switch n := apisixResp.Node.TNodes.(type) {
		case []interface{}:
			for _, node := range n {
				switch b := node.(type) {
				case map[string]interface{}:
					val, ok := b["value"]
					if ok {
						apisixResp.Node.Nodes = append(apisixResp.Node.Nodes, val.(map[string]interface{}))
					}
				}
			}
			break
		default:
			break
		}
	}
	return apisixResp, nil
}

var ApisixConfigTemplate = `
# Auto generate by https://github.com/anjia0532/discovery-syncer, Don't Modify

{{.Value}}
#END
`
var filePath = filepath.Join(os.TempDir(), "apisix.yaml")

func (apisixClient *ApisixClient) FetchAdminApiToFile() (string, string, error) {
	var tpl bytes.Buffer
	//"plugin_configs",
	uris := map[string]string{
		"routes":          "Routes",
		"services":        "Services",
		"upstreams":       "Upstreams",
		"plugins/list":    "Plugins",
		"ssl":             "Ssl",
		"global_rules":    "GlobalRules",
		"consumers":       "Consumers",
		"plugin_metadata": "PluginMetadata",
		"stream_routes":   "stream_routes",
	}

	apisixConfig := model.ApisixConfig{}
	for uri, field := range uris {
		apisixResp, err := apisixClient.fetchInfoFromApisix(uri)
		if err != nil {
			apisixClient.Logger.Errorf("[admin_api_to_yaml]fetchInfoFromApisix error,uri,%s,err:%s", uri, err)
			continue
		}
		v := reflect.ValueOf(&apisixConfig).Elem()
		if f := v.FieldByName(field); f.IsValid() {
			f.Set(reflect.ValueOf(apisixResp.Node.Nodes))
		}
	}

	jsonByte, err := json.Marshal(apisixConfig)
	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]convert struct to json error,err:%s", err)
		return "", "", err
	}

	ymlBytes, err := yaml.JSONToYAML(jsonByte)
	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]convert json to yaml error,err:%s", err)
		return "", "", err
	}

	tmpl, err := template.New("ApisixConfigTemplate").Parse(ApisixConfigTemplate)
	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]parse template error,err:%s", err)
		return "", "", err
	}

	value := map[string]string{"Value": string(ymlBytes)}
	err = tmpl.Execute(&tpl, value)
	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]template execute error,err:%s", err)
		return "", "", err
	}

	err = ioutil.WriteFile(filePath, tpl.Bytes(), 0644)
	if err != nil {
		apisixClient.Logger.Errorf("[admin_api_to_yaml]failed to write apisix.yaml ,err:%s", err)
		return "", "", err
	}
	return tpl.String(), filePath, nil
}
