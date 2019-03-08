package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"

	"github.com/bndr/gopencils"
)

type RestrictionOperation string

const (
	RestrictionEdit RestrictionOperation = `Edit`
	RestrictionView                      = `View`
)

type Restriction struct {
	User  string `json:"userName"`
	Group string `json:"groupName",omitempty`
}

type API struct {
	rest *gopencils.Resource

	// it's deprecated accordingly to Atlassian documentation,
	// but it's only way to set permissions
	json *gopencils.Resource
}

func NewAPI(baseURL string, username string, password string) *API {
	auth := &gopencils.BasicAuth{username, password}

	return &API{
		rest: gopencils.Api(baseURL+"/rest/api", auth),

		json: gopencils.Api(
			baseURL+"/rpc/json-rpc/confluenceservice-v2",
			auth,
		),
	}
}

func (api *API) findRootPage(space string) (*PageInfo, error) {
	page, err := api.findPage(space, ``)
	if err != nil {
		return nil, fmt.Errorf(
			`can't obtain first page from space '%s': %s`,
			space,
			err,
		)
	}

	if len(page.Ancestors) == 0 {
		return nil, fmt.Errorf(
			"page '%s' from space '%s' has no parents",
			page.Title,
			space,
		)
	}

	return &PageInfo{
		ID:    page.Ancestors[0].Id,
		Title: page.Ancestors[0].Title,
	}, nil
}

func (api *API) findPage(space string, title string) (*PageInfo, error) {

	result := struct {
		Results []PageInfo `json:"results"`
	}{}

	payload := map[string]string{
		"spaceKey": space,
		"expand":   "ancestors,version",
	}

	if title != "" {
		payload["title"] = title
	}

	request, err := api.rest.Res(
		"content/", &result,
	).Get(payload)

	if err != nil {
		return nil, err
	}

	if request.Raw.StatusCode == 401 {
		return nil, fmt.Errorf("authentification failed")
	}

	if request.Raw.StatusCode != 200 {
		return nil, fmt.Errorf(
			"Confluence REST API returns unexpected non-200 HTTP status: %s",
			request.Raw.Status,
		)
	}

	if len(result.Results) == 0 {
		return nil, nil
	}

	return &result.Results[0], nil
}

func (api *API) getPageByID(pageID string) (*PageInfo, error) {

	request, err := api.rest.Res(
		"content/"+pageID, &PageInfo{},
	).Get(map[string]string{"expand": "ancestors,version"})

	if err != nil {
		return nil, err
	}

	if request.Raw.StatusCode == 401 {
		return nil, fmt.Errorf("authentification failed")
	}

	if request.Raw.StatusCode == 404 {
		return nil, fmt.Errorf(
			"page with id '%s' not found, Confluence REST API returns 404",
			pageID,
		)
	}

	if request.Raw.StatusCode != 200 {
		return nil, fmt.Errorf(
			"Confluence REST API returns unexpected HTTP status: %s",
			request.Raw.Status,
		)
	}

	return request.Response.(*PageInfo), nil
}

func (api *API) createPage(
	space string,
	parent *PageInfo,
	title string,
	body string,
) (*PageInfo, error) {
	payload := map[string]interface{}{
		"type":  "page",
		"title": title,
		"space": map[string]interface{}{
			"key": space,
		},
		"body": map[string]interface{}{
			"storage": map[string]interface{}{
				"representation": "storage",
				"value":          body,
			},
		},
	}

	if parent != nil {
		payload["ancestors"] = []map[string]interface{}{
			{"id": parent.ID},
		}
	}

	request, err := api.rest.Res(
		"content/", &PageInfo{},
	).Post(payload)
	if err != nil {
		return nil, err
	}

	if request.Raw.StatusCode != 200 {
		output, _ := ioutil.ReadAll(request.Raw.Body)
		defer request.Raw.Body.Close()

		return nil, fmt.Errorf(
			"Confluence REST API returns unexpected non-200 HTTP status: %s, "+
				"output: %s",
			request.Raw.Status, output,
		)
	}

	return request.Response.(*PageInfo), nil
}

func (api *API) updatePage(
	page *PageInfo, newContent string,
) error {
	nextPageVersion := page.Version.Number + 1

	if len(page.Ancestors) == 0 {
		return fmt.Errorf(
			"page '%s' info does not contain any information about parents",
			page.ID,
		)
	}

	// picking only the last one, which is required by confluence
	oldAncestors := []map[string]interface{}{
		{"id": page.Ancestors[len(page.Ancestors)-1].Id},
	}

	payload := map[string]interface{}{
		"id":    page.ID,
		"type":  "page",
		"title": page.Title,
		"version": map[string]interface{}{
			"number":    nextPageVersion,
			"minorEdit": false,
		},
		"ancestors": oldAncestors,
		"body": map[string]interface{}{
			"storage": map[string]interface{}{
				"value":          string(newContent),
				"representation": "storage",
			},
		},
	}

	request, err := api.rest.Res(
		"content/"+page.ID, &map[string]interface{}{},
	).Put(payload)
	if err != nil {
		return err
	}

	if request.Raw.StatusCode != 200 {
		output, _ := ioutil.ReadAll(request.Raw.Body)
		defer request.Raw.Body.Close()

		return fmt.Errorf(
			"Confluence REST API returns unexpected non-200 HTTP status: %s, "+
				"output: %s",
			request.Raw.Status, output,
		)
	}

	return nil
}

func (api *API) setPagePermissions(
	page *PageInfo,
	operation RestrictionOperation,
	restrictions []Restriction,
) error {
	var result interface{}

	request, err := api.json.Res(
		"setContentPermissions", &result,
	).Post([]interface{}{
		page.ID,
		operation,
		restrictions,
	})
	if err != nil {
		return err
	}

	if request.Raw.StatusCode != 200 {
		output, _ := ioutil.ReadAll(request.Raw.Body)
		defer request.Raw.Body.Close()

		return fmt.Errorf(
			"Confluence JSON RPC returns unexpected non-200 HTTP status: %s, "+
				"output: %s",
			request.Raw.Status, output,
		)
	}

	if success, ok := result.(bool); !ok || !success {
		return fmt.Errorf(
			"'true' response expected, but '%v' encountered",
			result,
		)
	}

	return nil
}

func (api *API) attachFiles(page *PageInfo, files []string) error {
	result := struct {
		Results []PageInfo `json:"results"`
	}{}

	request, err := api.rest.Res(
		"content/"+page.ID+"/child/attachment", &result,
	).Get()

	if err != nil {
		return err
	}

	if request.Raw.StatusCode != 200 {
		output, _ := ioutil.ReadAll(request.Raw.Body)
		defer request.Raw.Body.Close()

		return fmt.Errorf(
			"Confluence REST API returns unexpected non-200 HTTP status: %s, "+
				"output: %s",
			request.Raw.Status, output,
		)
	}

	attachments := []string{}

	for _, file := range files {
		reattached := false

		for _, attachment := range result.Results {
			if file == attachment.Title {
				err = api.reattachFile(attachment.ID, page, file)
				if err != nil {
					return err
				}

				reattached = true
			}
		}

		if !reattached {
			attachments = append(attachments, file)
		}
	}

	if len(attachments) > 0 {
		err = api.attachManyFiles(page, attachments)

		if err != nil {
			return err
		}
	}

	return nil
}

func (api *API) reattachFile(
	id string, page *PageInfo, attachment string,
) error {
	var (
		payload = &bytes.Buffer{}
		writer  = multipart.NewWriter(payload)
	)

	file, err := os.Open(attachment)
	if err != nil {
		return err
	}

	content, err := writer.CreateFormFile("file", attachment)
	if err != nil {
		return err
	}

	io.Copy(content, file)

	writer.Close()

	result := struct {
		Results []PageInfo `json:"results"`
	}{}

	resource := api.rest.Res(
		"content/"+page.ID+"/child/attachment/"+id+"/data", &result,
	)
	resource.Payload = payload

	resource.Headers = http.Header{}
	resource.SetHeader("Content-Type", writer.FormDataContentType())
	resource.SetHeader("X-Atlassian-Token", "no-check")

	_, err = resource.Post()
	if err != nil {
		return err
	}

	return nil
}

func (api *API) attachManyFiles(page *PageInfo, files []string) error {
	var (
		payload = &bytes.Buffer{}
		writer  = multipart.NewWriter(payload)
	)

	for _, attachment := range files {
		file, err := os.Open(attachment)
		if err != nil {
			return err
		}

		content, err := writer.CreateFormFile("file", attachment)
		if err != nil {
			return err
		}

		io.Copy(content, file)
	}

	writer.Close()

	result := struct {
		Results []PageInfo `json:"results"`
	}{}

	resource := api.rest.Res(
		"content/"+page.ID+"/child/attachment", &result,
	)

	resource.Payload = payload
	resource.Headers = http.Header{}
	resource.SetHeader("Content-Type", writer.FormDataContentType())
	resource.SetHeader("X-Atlassian-Token", "no-check")

	request, err := resource.Post()
	if err != nil {
		return err
	}

	if request.Raw.StatusCode != 200 {
		output, _ := ioutil.ReadAll(request.Raw.Body)
		defer request.Raw.Body.Close()

		return fmt.Errorf(
			"Confluence REST API returns unexpected non-200 HTTP status: %s, "+
				"output: %s",
			request.Raw.Status, output,
		)
	}

	return nil
}
