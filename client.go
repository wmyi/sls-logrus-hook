package hook

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
)

const (
	DefaultTimeout           = 5 * time.Second
	MaxLogItemSize           = 512 * 1024      // Safe value for maximum 1M log item.
	MaxLogGroupSize          = 4 * 1024 * 1024 // Safe value for maximum 5M log group
	MaxLogBatchSize          = 1024            // Safe value for batch send size
)

type SlsClient struct {
	endpoint     string
	accessKey    string
	accessSecret string
	logStore     string
	lock         *sync.Mutex
	client       *http.Client
}

func NewSlsClient(endpoint string, accessKey string, accessSecret string, logStore string) (*SlsClient, error) {
	if len(endpoint) == 0 {
		return nil, errors.New("Sls endpoint should not be empty")
	}
	if len(accessKey) == 0 {
		return nil, errors.New("Sls access key should not be empty")
	}
	if len(accessSecret) == 0 {
		return nil, errors.New("Sls access secret should not be empty")
	}
	if len(logStore) == 0 {
		return nil, errors.New("Sls log store should not be empty")
	}
	return &SlsClient{
		endpoint:     endpoint,
		accessKey:    accessKey,
		accessSecret: accessSecret,
		logStore:     logStore,
		lock:         &sync.Mutex{},
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}, nil
}

func (client *SlsClient) Ping() error {
	// TODO use get log store to ping connection
	group := LogGroup{}
	group.Logs = []*Log{{
		Time: proto.Uint32(uint32(time.Now().Unix())),
		Contents: []*LogContent{{
			Key:   proto.String("__topic__"),
			Value: proto.String("status"),
		}, {
			Key:   proto.String("message"),
			Value: proto.String("Status check by sls-logrus-hook."),
		}},
	}}
	body, err := proto.Marshal(&group)
	if err != nil {
		return err
	}
	err = client.sendPb(body)
	if err != nil {
		return err
	}
	return nil
}

func (client *SlsClient) SendLogs(logs []*Log) error {
	if len(logs) == 0 {
		return nil
	}
	if len(logs) > MaxLogBatchSize {
		return errors.Errorf("Log batch size should not exceed %d.", MaxLogBatchSize)
	}
	group := LogGroup{}
	group.Logs = logs
	body, err := proto.Marshal(&group)
	if len(body) > MaxLogGroupSize {
		// Extreme cases when log group size exceed the maximum
		return client.splitSendLogs(logs)
	}
	if err != nil {
		return err
	}
	err = client.sendPb(body)
	if err != nil {
		return err
	}
	return nil
}

func logSize(log *Log) int {
	// Estimate log size
	size := 4
	for _, content := range log.Contents {
		size += len(*content.Key) + len(*content.Value) + 8
	}
	return size
}

func (client *SlsClient) splitSendLogs(logs []*Log) error {
	var errorList []error
	cursor := 0
	for cursor < len(logs) {
		groupSize := 0
		group := LogGroup{
			Logs: make([]*Log, 0),
		}
		for cursor < len(logs) {
			log := logs[cursor]
			size := logSize(log)
			if groupSize+size > MaxLogGroupSize {
				break
			}
			cursor++
			if size > MaxLogItemSize {
				// Print huge single log to stdout
				_, _ = fmt.Fprintf(os.Stdout, "[HUGE SLS LOG] %+v", log)
				continue
			}
			groupSize += size
			group.Logs = append(group.Logs, log)
		}

		body, err := proto.Marshal(&group)
		if len(body) > MaxLogGroupSize {
			// Extreme cases when log group size exceed the maximum
			_, _ = fmt.Fprintf(os.Stdout, "[HUGE SLS LOG GROUP] %+v", group)
			continue
		}
		if err != nil {
			errorList = append(errorList, err)
			continue
		}
		err = client.sendPb(body)
		if err != nil {
			errorList = append(errorList, err)
			continue
		}
	}
	if len(errorList) == 0 {
		return nil
	} else {
		return errors.Errorf("Fail to send logs due to the following errors: %+v", errorList)
	}
}

func (client *SlsClient) sendPb(logContent []byte) error {
	method := "POST"
	resource := "logstores/" + client.logStore + "/shards/lb"
	headers := make(map[string]string)
	logMD5 := md5.Sum(logContent)
	strMd5 := strings.ToUpper(fmt.Sprintf("%x", logMD5))

	headers[HeaderLogVersion] = SlsVersion
	headers[HeaderContentType] = "application/x-protobuf"
	headers[HeaderContentMd5] = strMd5
	headers[HeaderLogSignatureMethod] = SlsSignatureMethod
	headers[HeaderContentLength] = fmt.Sprintf("%v", len(logContent))
	headers[HeaderLogBodyRawSize] = "0"
	headers[HeaderHost] = client.endpoint

	headers[HeaderDate] = time.Now().UTC().Format(http.TimeFormat)

	if sign, e := ApiSign(client.accessSecret, method, headers, fmt.Sprintf("/%s", resource)); e != nil {
		return errors.WithMessage(e, "Fail to create sign for sls")
	} else {
		headers[HeaderAuthorization] = fmt.Sprintf("LOG %s:%s", client.accessKey, sign)
	}

	url := client.endpoint + "/" + resource
	if !strings.HasPrefix(client.endpoint, "http://") || strings.HasPrefix(client.endpoint, "https://") {
		url = "http://" + client.endpoint + "/" + resource
	}
	postBodyReader := bytes.NewBuffer(logContent)

	req, err := http.NewRequest(method, url, postBodyReader)
	if err != nil {
		return errors.WithMessage(err, "Error creating http request for sls")
	}
	for header, value := range headers {
		req.Header.Add(header, value)
	}

	resp, err := client.client.Do(req)
	if err != nil {
		return errors.WithMessage(err, "Error sending log with http client")
	}
	if resp.StatusCode != 200 {
		defer func() { _ = resp.Body.Close() }()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return errors.New(string(body))
	}
	return nil
}
