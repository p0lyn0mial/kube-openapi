/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package builder3

import (
	"encoding/json"
	"fmt"
	"github.com/go-openapi/spec"
	"net/http"
	"strings"

	"github.com/emicklei/go-restful"
	"github.com/p0lyn0mial/spec3"

	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/util"
)

const (
	OpenAPIVersion = "3.0"
	// TODO: Make this configurable.
	// extensionPrefix = "x-kubernetes-"
	// extensionV2Spec = extensionPrefix + "v2-spec", see https://github.com/kubernetes/kube-openapi/pull/166
)

type openAPI struct {
	config *common.Config
	spec   *spec3.OpenAPI
	//protocolList []string
	//definitions  map[string]common.OpenAPIDefinition
}

// BuildOpenAPISpec builds OpenAPI spec given a list of webservices (containing routes) and common.Config to customize it
func BuildOpenAPISpec(webServices []*restful.WebService, config *common.Config) (*spec3.OpenAPI, error) {
	o := newOpenAPI(config)
	err := o.buildOpenAPISpec(webServices)
	return o.spec, err
}

// newOpenAPI sets up the openAPI object so we can build the spec.
func newOpenAPI(config *common.Config) openAPI {
	o := openAPI{
		config: config,
		spec: &spec3.OpenAPI{
			Paths: &spec3.Paths{Paths: map[string]*spec3.Path{}},
		},
	}
	if o.config.GetOperationIDAndTags == nil {
	}
	if o.config.GetDefinitionName == nil {
		// TODO: change the name to GVK ??!!
	}
	if o.config.CommonResponses == nil {
	}
	return o
}

func (o *openAPI) buildOpenAPISpec(webServices []*restful.WebService) error {
	pathsToIgnore := util.NewTrie(o.config.IgnorePrefixes)
	for _, w := range webServices {
		rootPath := w.RootPath()
		if pathsToIgnore.HasPrefix(rootPath) {
			continue
		}
		commonParams, err := o.buildParameters(w.PathParameters())
		if err != nil {
			return err
		}

		for path, routes := range groupRoutesByPath(w.Routes()) {
			// go-swagger has special variable definition {$NAME:*} that can only be
			// used at the end of the path and it is not recognized by OpenAPI.
			if strings.HasSuffix(path, ":*}") {
				path = path[:len(path)-3] + "}"
			}
			if pathsToIgnore.HasPrefix(path) {
				continue
			}

			// Aggregating common parameters make API spec (and generated clients) simpler
			inPathCommonParamsMap, err := o.findCommonParameters(routes)
			if err != nil {
				return err
			}
			pathItem, exists := o.spec.Paths.Paths[path]
			if exists {
				return fmt.Errorf("duplicate webservice route has been found for path: %v", path)
			}
			pathItem = &spec3.Path{
				PathProps: spec3.PathProps{
					Parameters: make([]*spec3.Parameter, 0),
				},
			}
			// add web services's parameters as well as any parameters appears in all ops, as common parameters
			pathItem.Parameters = append(pathItem.Parameters, commonParams...)
			for _, p := range inPathCommonParamsMap {
				pathItem.Parameters = append(pathItem.Parameters, p)
			}
			sortParameters(pathItem.Parameters)

			for _, route := range routes {
				// TODO: build operations
				_ = route
			}

			o.spec.Paths.Paths[path] = pathItem
		}
	}

	return nil
}

func (o *openAPI) buildParameters(restParam []*restful.Parameter) (ret []*spec3.Parameter, err error) {
	ret = make([]*spec3.Parameter, len(restParam))
	for i, v := range restParam {
		ret[i], err = o.buildParameter(v.Data(), nil)
		if err != nil {
			return ret, err
		}
	}
	return ret, nil
}

func (o *openAPI) buildParameter(restParam restful.ParameterData, bodySample interface{}) (ret *spec3.Parameter, err error) {
	ret = &spec3.Parameter{
		ParameterProps: spec3.ParameterProps{
			Name:        restParam.Name,
			Description: restParam.Description,
			Required:    restParam.Required,
		},
	}
	switch restParam.Kind {
	case restful.PathParameterKind:
		ret.In = "path"
		if !restParam.Required {
			return ret, fmt.Errorf("path parameters should be marked at required for parameter %v", restParam)
		}
	case restful.QueryParameterKind:
		ret.In = "query"
	case restful.HeaderParameterKind:
		ret.In = "header"
	/* TODO: add support for the cookie param */
	default:
		return ret, fmt.Errorf("unsupported restful parameter kind : %v", restParam.Kind)
	}

	return ret, nil
}


// buildOperations builds operations for each webservice path
func (o *openAPI) buildOperations(route restful.Route, inPathCommonParamsMap map[interface{}]*spec3.Parameter) (ret *spec3.Operation, err error) {
	ret = &spec3.Operation{
		OperationProps: spec3.OperationProps{
			Description: route.Doc,
			//Consumes:    route.Consumes,
			Produces:    route.Produces,
			Schemes:     o.config.ProtocolList,
			Responses: &spec3.Responses{
				ResponsesProps: spec3.ResponsesProps{
					StatusCodeResponses: make(map[int]*spec3.Response),
				},
			},
		},
	}
	for k, v := range route.Metadata {
		if strings.HasPrefix(k, extensionPrefix) {
			if ret.Extensions == nil {
				ret.Extensions = spec.Extensions{}
			}
			ret.Extensions.Add(k, v)
		}
	}
	if ret.ID, ret.Tags, err = o.config.GetOperationIDAndTags(&route); err != nil {
		return ret, err
	}

	// Build responses
	for _, resp := range route.ResponseErrors {
		ret.Responses.StatusCodeResponses[resp.Code], err = o.buildResponse(resp.Model, resp.Message, route.Consumes)
		if err != nil {
			return ret, err
		}
	}
	// If there is no response but a write sample, assume that write sample is an http.StatusOK response.
	if len(ret.Responses.StatusCodeResponses) == 0 && route.WriteSample != nil {
		ret.Responses.StatusCodeResponses[http.StatusOK], err = o.buildResponse(route.WriteSample, "OK", route.Consumes)
		if err != nil {
			return ret, err
		}
	}
	for code, resp := range o.config.CommonResponses {
		if _, exists := ret.Responses.StatusCodeResponses[code]; !exists {
			ret.Responses.StatusCodeResponses[code] = resp
		}
	}
	// If there is still no response, use default response provided.
	if len(ret.Responses.StatusCodeResponses) == 0 {
		ret.Responses.Default = o.config.DefaultResponse
	}

	// Build non-common Parameters
	ret.Parameters = make([]*spec3.Parameter, 0)
	for _, param := range route.ParameterDocs {
		if _, isCommon := inPathCommonParamsMap[mapKeyFromParam(param)]; !isCommon {
			openAPIParam, err := o.buildParameter(param.Data(), route.ReadSample)
			if err != nil {
				return ret, err
			}
			ret.Parameters = append(ret.Parameters, openAPIParam)
		}
	}
	return ret, nil
}

func (o *openAPI) buildResponse(model interface{}, description string, mediaTypes []string) (*spec3.Response, error) {
	schema, err := o.toSchema(util.GetCanonicalTypeName(model))
	if err != nil {
		return nil, err
	}
	ret := &spec3.Response{
		ResponseProps: spec3.ResponseProps{
			Description: description,
			Content: map[string]*spec3.MediaType{},
		},
	}

	for  _, mediaType := range mediaTypes {
		if _, exists := ret.Content[mediaType]; exists {
			return nil, fmt.Errorf("duplicate meida type %v for %v", mediaType, util.GetCanonicalTypeName(model))
		}
		ret.Content[mediaType] = &spec3.MediaType{
			MediaTypeProps: spec3.MediaTypeProps{
				Schema:      schema,
			},
		}
	}

	return ret, nil
}

// buildDefinitionForType build a definition for a given type and return a referable name to its definition.
// This is the main function that keep track of definitions used in this spec and is depend on code generated
// by k8s.io/kubernetes/cmd/libs/go2idl/openapi-gen.
func (o *openAPI) buildDefinitionForType(name string) (string, error) {
	if err := o.buildDefinitionRecursively(name); err != nil {
		return "", err
	}
	defName, _ := o.config.GetDefinitionName(name)
	return "#/components/schemas/" + common.EscapeJsonPointer(defName), nil
}


func (o *openAPI) buildDefinitionRecursively(name string) error {
	uniqueName, extensions := o.config.GetDefinitionName(name)
	if _, ok := o.spec.Components.Schemas[uniqueName]; ok {
		return nil
	}
	if item, ok := o.definitions[name]; ok {
		schema := &spec.Schema{
			VendorExtensible:   item.Schema.VendorExtensible,
			SchemaProps:        item.Schema.SchemaProps,
			SwaggerSchemaProps: item.Schema.SwaggerSchemaProps,
		}
		if extensions != nil {
			if schema.Extensions == nil {
				schema.Extensions = spec.Extensions{}
			}
			for k, v := range extensions {
				schema.Extensions[k] = v
			}
		}
		o.spec.Components.Schemas[uniqueName] = schema
		for _, v := range item.Dependencies {
			if err := o.buildDefinitionRecursively(v); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("cannot find model definition for %v. If you added a new type, you may need to add +k8s:openapi-gen=true to the package or type and run code-gen again", name)
	}
	return nil
}

func (o *openAPI) toSchema(name string) (_ *spec.Schema, err error) {
	if openAPIType, openAPIFormat := common.GetOpenAPITypeFormat(name); openAPIType != "" {
		return &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type:   []string{openAPIType},
				Format: openAPIFormat,
			},
		}, nil
	} else {
		ref, err := o.buildDefinitionForType(name)
		if err != nil {
			return nil, err
		}
		return &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Ref: spec.MustCreateRef(ref),
			},
		}, nil
	}
}

// TODO: could be moved to util.go
func (o *openAPI) findCommonParameters(routes []restful.Route) (map[interface{}]*spec3.Parameter, error) {
	commonParamsMap := make(map[interface{}]*spec3.Parameter, 0)
	paramOpsCountByName := make(map[interface{}]int, 0)
	paramNameKindToDataMap := make(map[interface{}]restful.ParameterData, 0)
	for _, route := range routes {
		routeParamDuplicateMap := make(map[interface{}]bool)
		s := ""
		for _, param := range route.ParameterDocs {
			m, _ := json.Marshal(param.Data())
			s += string(m) + "\n"
			key := mapKeyFromParam(param)
			if routeParamDuplicateMap[key] {
				msg, _ := json.Marshal(route.ParameterDocs)
				return commonParamsMap, fmt.Errorf("duplicate parameter %v for route %v, %v", param.Data().Name, string(msg), s)
			}
			routeParamDuplicateMap[key] = true
			paramOpsCountByName[key]++
			paramNameKindToDataMap[key] = param.Data()
		}
	}
	for key, count := range paramOpsCountByName {
		paramData := paramNameKindToDataMap[key]
		if count == len(routes) && paramData.Kind != restful.BodyParameterKind {
			openAPIParam, err := o.buildParameter(paramData, nil)
			if err != nil {
				return commonParamsMap, err
			}
			commonParamsMap[key] = openAPIParam
		}
	}
	return commonParamsMap, nil
}

