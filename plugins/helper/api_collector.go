package helper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"text/template"

	"github.com/merico-dev/lake/plugins/core"
	"gorm.io/datatypes"
)

type Pager struct {
	Page int
	Skip int
	Size int
}

type RequestData struct {
	Pager  *Pager
	Params interface{}
	Input  interface{}
}

type AsyncResponseHandler func(res *http.Response) error

type ApiCollectorArgs struct {
	RawDataSubTaskArgs
	/*
		url may use arbitrary variables from different source in any order, we need GoTemplate to allow more
		flexible for all kinds of possibility.
		Pager contains information for a particular page, calculated by ApiCollector, and will be passed into
		GoTemplate to generate a url for that page.
		We want to do page-fetching in ApiCollector, because the logic are highly similar, by doing so, we can
		avoid duplicate logic for every tasks, and when we have a better idea like improving performance, we can
		do it in one place
	*/
	UrlTemplate string `comment:"GoTemplate for API url"`
	// (Optional) Return query string for request, or you can plug them into UrlTemplate directly
	Query func(reqData *RequestData) (url.Values, error) `comment:"Extra query string when requesting API, like 'Since' option for jira issues collection"`
	// Some api might do pagination by http headers
	Header      func(reqData *RequestData) (http.Header, error)
	PageSize    int
	Incremental bool `comment:"Indicate this is a incremental collection, so the existing data won't get flushed"`
	ApiClient   core.AsyncApiClient
	/*
		Sometimes, we need to collect data based on previous collected data, like jira changelog, it requires
		issue_id as part of the url.
		We can mimic `stdin` design, to accept a `Input` function which produces a `Iterator`, collector
		should iterate all records, and do data-fetching for each on, either in parallel or sequential order
		UrlTemplate: "api/3/issue/{{ Input.ID }}/changelog"
	*/
	Input          Iterator
	InputRateLimit int
	/*
		For api endpoint that returns number of total pages, ApiCollector can collect pages in parallel with ease,
		or other techniques are required if this information was missing.
	*/
	GetTotalPages  func(res *http.Response, args *ApiCollectorArgs) (int, error)
	Concurrency    int
	ResponseParser func(res *http.Response) ([]json.RawMessage, error)
}

type ApiCollector struct {
	*RawDataSubTask
	args        *ApiCollectorArgs
	urlTemplate *template.Template
}

// NewApiCollector allocates a new ApiCollector  with the given args.
// ApiCollector can help you collecting data from some api with ease, pass in a AsyncApiClient and tell it which part
// of response you want to save, ApiCollector will collect them from remote server and store them into database.
func NewApiCollector(args ApiCollectorArgs) (*ApiCollector, error) {
	// process args
	rawDataSubTask, err := newRawDataSubTask(args.RawDataSubTaskArgs)
	if err != nil {
		return nil, err
	}
	// TODO: check if args.Table is valid
	if args.UrlTemplate == "" {
		return nil, fmt.Errorf("UrlTemplate is required")
	}
	tpl, err := template.New(args.Table).Parse(args.UrlTemplate)
	if err != nil {
		return nil, fmt.Errorf("Failed to compile UrlTemplate: %w", err)
	}
	if args.ApiClient == nil {
		return nil, fmt.Errorf("ApiClient is required")
	}
	if args.ResponseParser == nil {
		return nil, fmt.Errorf("ResponseParser is required")
	}
	if args.InputRateLimit == 0 {
		args.InputRateLimit = 50
	}
	if args.Concurrency < 1 {
		args.Concurrency = 1
	}
	return &ApiCollector{
		RawDataSubTask: rawDataSubTask,
		args:           &args,
		urlTemplate:    tpl,
	}, nil
}

// Start collection
func (collector *ApiCollector) Execute() error {
	logger := collector.args.Ctx.GetLogger()
	logger.Info("start api collection")

	// make sure table is created
	db := collector.args.Ctx.GetDb()
	err := db.Table(collector.table).AutoMigrate(&RawData{})
	if err != nil {
		return err
	}

	// flush data if not incremental collection
	if !collector.args.Incremental {
		err = db.Table(collector.table).Delete(&RawData{}, "params = ?", collector.params).Error
		if err != nil {
			return err
		}
	}

	if collector.args.Input != nil {
		collector.args.Ctx.SetProgress(0, -1)
		// load all rows from iterator, and do multiple `exec` accordingly
		// TODO: this loads all records into memory, we need lazy-load
		iterator := collector.args.Input
		defer iterator.Close()
		for iterator.HasNext() {
			input, err := iterator.Fetch()
			if err != nil {
				return err
			}
			err = collector.exec(input)
			if err != nil {
				break
			}
		}

	} else {
		// or we just did it once
		err = collector.exec(nil)
	}

	collector.args.ApiClient.WaitAsync()
	logger.Info("end api collection")
	return err
}

func (collector *ApiCollector) exec(input interface{}) error {
	reqData := new(RequestData)
	reqData.Input = input
	if collector.args.PageSize > 0 {
		// collect multiple pages
		return collector.fetchPagesAsync(reqData)
	}
	// collect detail of a record
	//return collector.fetchAsync(reqData, collector.handleResponse)
	return collector.fetchAsync(reqData, collector.handleNoPageResponse(reqData))
}

func (collector *ApiCollector) generateUrl(pager *Pager, input interface{}) (string, error) {
	var buf bytes.Buffer
	err := collector.urlTemplate.Execute(&buf, &RequestData{
		Pager:  pager,
		Params: collector.args.Params,
		Input:  input,
	})
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (collector *ApiCollector) fetchPagesAsync(reqData *RequestData) error {
	var err error
	if collector.args.GetTotalPages != nil {
		/* when total pages is available from api*/
		// fetch the very first page
		err = collector.fetchAsync(reqData, func(res *http.Response) error {
			// gather total pages
			body, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return err
			}
			res.Body.Close()
			res.Body = ioutil.NopCloser(bytes.NewBuffer(body))
			totalPages, err := collector.args.GetTotalPages(res, collector.args)
			if err != nil {
				return err
			}
			// save response body of first page
			res.Body = ioutil.NopCloser(bytes.NewBuffer(body))
			err = collector.handleResponse(res)
			if err != nil {
				return err
			}
			if collector.args.Input == nil {
				collector.args.Ctx.SetProgress(1, totalPages)
			}
			// fetch other pages in parallel
			for page := 2; page <= totalPages; page++ {
				reqDataTemp := &RequestData{
					Pager: &Pager{
						Page: page,
						Size: collector.args.PageSize,
						Skip: collector.args.PageSize * (page - 1),
					},
					Input: reqData.Input,
				}
				err = collector.fetchAsync(reqDataTemp, func(res *http.Response) error {
					err := collector.handleResponse(res)
					if err != nil {
						return err
					}
					if collector.args.Input == nil {
						collector.args.Ctx.IncProgress(1)
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			return nil
		})
	} else {
		// if api doesn't return total number of pages, employ a step concurrent technique
		// when `Concurrency` was set to 3:
		// goroutine #1 fetches pages 1/4/7..
		// goroutine #2 fetches pages 2/5/8...
		// goroutine #3 fetches pages 3/6/9...
		for i := 0; i < collector.args.Concurrency; i++ {
			reqDataTemp := &RequestData{
				Pager: &Pager{
					Page: i + 1,
					Size: collector.args.PageSize,
					Skip: collector.args.PageSize * (i),
				},
				Input: reqData.Input,
			}
			err = collector.fetchAsync(reqDataTemp, collector.recursive(reqDataTemp))
			if err != nil {
				return err
			}
		}
	}
	if err != nil {
		return err
	}
	if collector.args.Input != nil {
		collector.args.Ctx.IncProgress(1)
	}
	return nil
}

func (collector *ApiCollector) handleNoPageResponse(reqData *RequestData) func(res *http.Response) error {
	return func(res *http.Response) error {
		_, err := collector.saveRawData(res, reqData.Input)
		if err != nil{
			return err
		}
		return nil
	}
}

func (collector *ApiCollector) handleResponse(res *http.Response) error {
	_, err := collector.saveRawData(res, nil)
	return err
}
func (collector *ApiCollector) saveRawData(res *http.Response, input interface{}) (int, error) {
	items, err := collector.args.ResponseParser(res)
	if err != nil {
		return 0, err
	}
	res.Body.Close()

	inputJson, _ := json.Marshal(input)

	if len(items) == 0 {
		return 0, nil
	}
	db := collector.args.Ctx.GetDb()
	u := res.Request.URL.String()
	dd := make([]*RawData, len(items))
	for i, msg := range items {
		dd[i] = &RawData{
			Params: collector.params,
			Data:   datatypes.JSON(msg),
			Url:    u,
			Input:  inputJson,
		}
	}
	return len(dd), db.Table(collector.table).Create(dd).Error
}

func (collector *ApiCollector) recursive(reqData *RequestData) func(res *http.Response) error {
	return func(res *http.Response) error {
		count, err := collector.saveRawData(res, reqData.Input)
		if err != nil {
			return err
		}
		if count < collector.args.PageSize {
			return nil
		}
		reqData.Pager.Skip += collector.args.PageSize * reqData.Pager.Page
		reqData.Pager.Page += collector.args.Concurrency
		return collector.fetchAsync(reqData, collector.recursive(reqData))
	}
}

func (collector *ApiCollector) fetchAsync(reqData *RequestData, handler func(*http.Response) error) error {
	if reqData.Pager == nil {
		reqData.Pager = &Pager{
			Page: 1,
			Size: 100,
			Skip: 0,
		}
	}
	apiUrl, err := collector.generateUrl(reqData.Pager, reqData.Input)
	if err != nil {
		return err
	}
	var apiQuery url.Values
	if collector.args.Query != nil {
		apiQuery, err = collector.args.Query(reqData)
		if err != nil {
			return err
		}
	}
	apiHeader := (http.Header)(nil)
	if collector.args.Header != nil {
		apiHeader, err = collector.args.Header(reqData)
		if err != nil {
			return err
		}
	}
	return collector.args.ApiClient.GetAsync(apiUrl, apiQuery, apiHeader, handler)
}

func GetRawMessageDirectFromResponse(res *http.Response) ([]json.RawMessage, error) {
	body, err := ioutil.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{body}, nil
}

var _ core.SubTask = (*ApiCollector)(nil)