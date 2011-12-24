// Copyright 2011 Phus Lu. All rights reserved.

package fetch

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"regexp"
	"strconv"
	"strings"
	"time"

	"appengine"
	"appengine/urlfetch"
	"http"
)

const (
	Version  = "1.7.0"
	Author   = "phus.lu@gmail.com"
	Password = ""

	FetchMax     = 3
	FetchMaxSize = 1024 * 1024
	Deadline     = 30
)

func encodeData(dic map[string]string) []byte {
	w := bytes.NewBufferString("")
	for k, v := range dic {
		fmt.Fprintf(w, "&%s=%s", k, hex.EncodeToString([]byte(v)))
	}
	return w.Bytes()[1:]
}

func decodeData(qs []byte) map[string]string {
	m := make(map[string]string)
	for _, kv := range strings.Split(string(qs), "&") {
		if kv != "" {
			pair := strings.Split(kv, "=")
			value, _ := hex.DecodeString(pair[1])
			m[pair[0]] = string(value)
		}
	}
	return m
}

type Webapp struct {
	response http.ResponseWriter
	request  *http.Request
	context  appengine.Context
}

func (app Webapp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.response = w
	app.request = r
	app.context = appengine.NewContext(app.request)
	if r.Method == "POST" {
		app.post()
	} else {
		app.get()
	}
}

func (app Webapp) printResponse(status int, header map[string]string, content []byte) {
	headerBytes := encodeData(header)

	app.response.WriteHeader(200)
	app.response.Header().Set("Content-Type", "image/gif")

	if contentType, ok := header["Content-Type"]; ok && strings.HasPrefix(contentType, "text/") {
		app.response.Write([]byte("1"))
		w, err := zlib.NewWriter(app.response)
		if err != nil {
			app.context.Criticalf("zlib.NewWriter(app.response) Error: %v", err)
			return
		}
		defer w.Close()
		binary.Write(w, binary.BigEndian, uint32(status))
		binary.Write(w, binary.BigEndian, uint32(len(headerBytes)))
		binary.Write(w, binary.BigEndian, uint32(len(content)))
		w.Write(headerBytes)
		w.Write(content)
	} else {
		app.response.Write([]byte("0"))
		binary.Write(app.response, binary.BigEndian, uint32(status))
		binary.Write(app.response, binary.BigEndian, uint32(len(headerBytes)))
		binary.Write(app.response, binary.BigEndian, uint32(len(content)))
		app.response.Write(headerBytes)
		app.response.Write(content)
	}
}

func (app Webapp) printNotify(method string, url string, status int, text string) {
	content := []byte(fmt.Sprintf("<h2>GAE/GO Fetch Server Info</h2><hr noshade='noshade'><p>%s '%s'</p><p>Return Code: %d</p><p>Message: %s</p>", method, url, status, text))
	headers := map[string]string{"Content-Type": "text/html"}
	app.printResponse(status, headers, content)
}

func (app Webapp) post() {
	r, err := zlib.NewReader(app.request.Body)
	if err != nil {
		app.context.Criticalf("zlib.NewReader(app.request.Body) Error: %v", err)
		return
	}
	defer r.Close()
	data, err := ioutil.ReadAll(r)
	if err != nil {
		app.context.Criticalf("ioutil.ReadAll(r) Error: %v", err)
		return
	}
	request := decodeData(data)

	method := request["method"]
	url := request["url"]
	headers := request["headers"]

	if Password != "" {
		password, ok := request["password"]
		if !ok || password != Password {
			app.printNotify(method, url, 403, "Wrong Password.")
		}
	}

	if !strings.HasPrefix(url, "http") {
		app.printNotify(method, url, 501, "Unsupported Scheme")
	}

	payload := strings.NewReader(request["payload"])
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		app.printNotify(method, url, 500, "http.NewRequest(method, url, payload) failed")
	}

	for _, line := range strings.Split(headers, "\r\n") {
		kv := strings.SplitN(line, ":", 2)
		if len(kv) == 2 {
			req.Header.Set(strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1]))
		}
	}

	deadline := float64(Deadline)
	var errors []string
	for i := 0; i < FetchMax; i++ {
		t := &urlfetch.Transport{app.context, deadline, true}
		resp, err := t.RoundTrip(req)
		if err != nil {
			message := err.String()
			errors = append(errors, message)
			if strings.Contains(message, "DEADLINE_EXCEEDED") {
				app.context.Errorf("URLFetchServiceError_DEADLINE_EXCEEDED(deadline=%s, url=%v)", deadline, url)
				time.Sleep(1)
				deadline *= 2
			} else if strings.Contains(message, "FETCH_ERROR") {
				app.context.Errorf("URLFetchServiceError_FETCH_ERROR(deadline=%s, url=%v)", deadline, url)
				time.Sleep(1)
				deadline *= 2
			} else if strings.Contains(message, "INVALID_URL") {
				app.printNotify(method, url, 501, fmt.Sprintf("Invalid URL: %s", err.String()))
				return
			} else if strings.Contains(message, "RESPONSE_TOO_LARGE") {
				app.context.Errorf("URLFetchServiceError_RESPONSE_TOO_LARGE(url=%v)", url)
				req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", FetchMaxSize))
				deadline *= 2
			} else {
				app.context.Errorf("URLFetchServiceError_UNKOWN(url=%v, error=%v)", url, err)
				time.Sleep(4)
			}
			continue
		}

		status := resp.StatusCode
		header := make(map[string]string)
		for k, vv := range resp.Header {
			if strings.ToLower(k) != "set-cookie" {
				header[k] = vv[0]
			} else {
				var cookies []string
				i := -1
				regex, _ := regexp.Compile("^[^ =]+ ")
				for _, sc := range strings.Split(vv[0], ", ") {
					if 0 <= i && regex.MatchString(sc) {
						cookies[i] = fmt.Sprintf("%s, %s", cookies[i], sc)
					} else {
						cookies = append(cookies, sc)
						i += 1
					}
				}
				header["Set-Cookie"] = strings.Join(cookies, "\r\nSet-Cookie: ")
			}
		}

		content, err := ioutil.ReadAll(resp.Body)
		if err == urlfetch.ErrTruncatedBody {
			app.context.Criticalf("ioutil.ReadAll(resp.Body) return urlfetch.ErrTruncatedBody")
		}
		if status == 206 {
			header["Accept-Ranges"] = "bytes"
			header["Content-Length"] = strconv.Itoa(len(content))
		}
		header["Connection"] = "close"

		//app.printNotify(method, url, 502, fmt.Sprintf("status=%d, header=%v, len(content)=%d", status, resp.Header, len(content)))
		app.printResponse(status, header, content)
		return
	}
	app.printNotify(method, url, 502, fmt.Sprintf("Fetch Server Failed: %v", errors))
}

func (app Webapp) get() {
	app.response.WriteHeader(http.StatusOK)
	app.response.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(app.response, `
<html>
<head>
    <link rel="icon" type="image/vnd.microsoft.icon" href="http://www.google.cn/favicon.ico">
    <meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
    <title>GoAgent %s 已经在工作了</title>
</head>
<body>
    <table width="800" border="0" align="center">
        <tr><td align="center"><hr></td></tr>
        <tr><td align="center">
            <b><h1>GoAgent %s 已经在工作了</h1></b>
        </td></tr>
        <tr><td align="center"><hr></td></tr>

        <tr><td align="center">
            GoAgent是一个开源的HTTP Proxy软件,使用Go/Python编写,运行于Google App Engine平台上.
        </td></tr>
        <tr><td align="center"><hr></td></tr>

        <tr><td align="center">
            更多相关介绍,请参考<a href="http://code.google.com/p/goagent/">GoAgent项目主页</a>.
        </td></tr>
        <tr><td align="center"><hr></td></tr>

    </table>
</body>
</html>`, Version, Version)
}

func init() {
	http.Handle("/fetch.py", Webapp{})
}
