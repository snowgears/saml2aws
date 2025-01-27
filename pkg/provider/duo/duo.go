package duo

import (
	"crypto/tls"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/versent/saml2aws/pkg/cfg"
	"github.com/versent/saml2aws/pkg/creds"
	"github.com/versent/saml2aws/pkg/prompter"
	"github.com/versent/saml2aws/pkg/provider"
)

var logger = logrus.WithField("provider", "duo")

// Client wrapper around Duo Access Gateway enabling authentication and retrieval of assertions
type Client struct {
	client     *provider.HTTPClient
	idpAccount *cfg.IDPAccount
}

// New create a new Shibboleth client
func New(idpAccount *cfg.IDPAccount) (*Client, error) {

	tr := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: idpAccount.SkipVerify, Renegotiation: tls.RenegotiateFreelyAsClient},
	}

	client, err := provider.NewHTTPClient(tr)
	if err != nil {
		return nil, errors.Wrap(err, "error building http client")
	}

	return &Client{
		client:     client,
		idpAccount: idpAccount,
	}, nil
}

// Authenticate authenticate to Duo Access Gateway and return the data from the body of the SAML assertion.
func (dc *Client) Authenticate(loginDetails *creds.LoginDetails) (string, error) {

	var authSubmitURL string
	var samlAssertion string

	//https://tanner.duoselab.com/dag/saml2/idp/SSOService.php?spentityid=DI8ESCQGSFOJRBUQSBVI

	//duoURL := fmt.Sprintf("%s/idp/SSOService.php?spentityid=DI8ESCQGSFOJRBUQSBVI", loginDetails.URL)

	duoURL := fmt.Sprintf("%s/dag/saml2/idp/SSOService.php?spentityid=DI8ESCQGSFOJRBUQSBVI", loginDetails.URL)//, dc.idpAccount.AmazonWebservicesURN)

	//fmt.Println(duoURL)

	res, err := dc.client.Get(duoURL)
	if err != nil {
		return samlAssertion, errors.Wrap(err, "error retrieving form")
	}

	fmt.Println(res)

	resData,err := ioutil.ReadAll(res.Body)
	if err != nil {
    	//log.Fatal(err)
	}
	resString := string(resData)
	fmt.Println(resString)


	doc, err := goquery.NewDocumentFromResponse(res)
	if err != nil {
		return samlAssertion, errors.Wrap(err, "failed to build document from response")
	}

	//fmt.Println(doc)

	//var authUrlString string := string(doc.Url)

	authUrlString := fmt.Sprint(doc.Url)

	//i now have the url where the actual auth form is on the DAG
	//fmt.Println(authUrlString)

	authForm := url.Values{}

	fmt.Println(authForm)


	doc.Find("input").Each(func(i int, s *goquery.Selection) {
		updateFormData(authForm, s, loginDetails)
	})

	doc.Find("form").Each(func(i int, s *goquery.Selection) {
		action, ok := s.Attr("action")
		if !ok {
			return
		}
		fmt.Println(action) //TODO
		//authSubmitURL = action //TODO PUT THIS BACK
		authSubmitURL = authUrlString
	})

	authSubmitURL = authUrlString

	if authSubmitURL == "" {
		return samlAssertion, fmt.Errorf("unable to locate IDP authentication form submit URL")
	}

	fmt.Println(authSubmitURL)

	req, err := http.NewRequest("POST", authSubmitURL, strings.NewReader(authForm.Encode()))
	if err != nil {
		return samlAssertion, errors.Wrap(err, "error building authentication request")
	}

	fmt.Println("POSTED THE AUTH FORM DAG")

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.URL.Host = res.Request.URL.Host
	req.URL.Scheme = res.Request.URL.Scheme

	res, err = dc.client.Do(req)
	if err != nil {
		return samlAssertion, errors.Wrap(err, "error retrieving login form results")
	}

	switch dc.idpAccount.MFA {
	case "Auto":
		b, _ := ioutil.ReadAll(res.Body)

		mfaRes, err := verifyMfa(dc, loginDetails.URL, string(b))
		if err != nil {
			return mfaRes.Status, errors.Wrap(err, "error verifying MFA")
		}

		res = mfaRes

	}

	samlAssertion, err = extractSamlResponse(res)
	if err != nil {
		return samlAssertion, errors.Wrap(err, "error extracting SAMLResponse blob from final Duo Access Gateway response")
	}

	return samlAssertion, nil
}

func updateFormData(authForm url.Values, s *goquery.Selection, user *creds.LoginDetails) {
	name, ok := s.Attr("name")
	authForm.Add("_eventId_proceed", "")

	if !ok {
		return
	}
	lname := strings.ToLower(name)
	if strings.Contains(lname, "user") {
		authForm.Add(name, user.Username)
	} else if strings.Contains(lname, "email") {
		authForm.Add(name, user.Username)
	} else if strings.Contains(lname, "pass") {
		authForm.Add(name, user.Password)
	} else {
		// pass through any hidden fields
		val, ok := s.Attr("value")
		if !ok {
			return
		}
		authForm.Add(name, val)
	}
}

func verifyMfa(oc *Client, shibbolethHost string, resp string) (*http.Response, error) {

	duoHost, postAction, tx, app := parseTokens(resp)

	parent := fmt.Sprintf(shibbolethHost + postAction)

	duoTxCookie, err := verifyDuoMfa(oc, duoHost, parent, tx)
	if err != nil {
		return nil, errors.Wrap(err, "error when interacting with Duo iframe")
	}

	idpForm := url.Values{}
	idpForm.Add("_eventId", "proceed")
	idpForm.Add("sig_response", duoTxCookie+":"+app)

	req, err := http.NewRequest("POST", parent, strings.NewReader(idpForm.Encode()))
	if err != nil {
		return nil, errors.Wrap(err, "error posting multi-factor verification to duo server")
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err := oc.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving verify response")
	}

	return res, nil
}

func verifyDuoMfa(oc *Client, duoHost string, parent string, tx string) (string, error) {
	// initiate duo mfa to get sid
	duoSubmitURL := fmt.Sprintf("https://%s/frame/web/v1/auth", duoHost)

	duoForm := url.Values{}
	duoForm.Add("parent", parent)
	duoForm.Add("java_version", "")
	duoForm.Add("java_version", "")
	duoForm.Add("flash_version", "")
	duoForm.Add("screen_resolution_width", "3008")
	duoForm.Add("screen_resolution_height", "1692")
	duoForm.Add("color_depth", "24")

	req, err := http.NewRequest("POST", duoSubmitURL, strings.NewReader(duoForm.Encode()))
	if err != nil {
		return "", errors.Wrap(err, "error building authentication request")
	}
	q := req.URL.Query()
	q.Add("tx", tx)
	req.URL.RawQuery = q.Encode()

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err := oc.client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "error retrieving verify response")
	}

	//try to extract sid
	doc, err := goquery.NewDocumentFromResponse(res)
	if err != nil {
		return "", errors.Wrap(err, "error parsing document")
	}

	duoSID, ok := doc.Find("input[name=\"sid\"]").Attr("value")
	if !ok {
		return "", errors.Wrap(err, "unable to locate saml response")
	}
	duoSID = html.UnescapeString(duoSID)

	//prompt for mfa type
	//supporting push, call, and passcode for now

	var token string

	var duoMfaOptions = []string{
		"Duo Push",
		"Phone Call",
		"Passcode",
	}

	duoMfaOption := prompter.Choose("Select a DUO MFA Option", duoMfaOptions)

	if duoMfaOptions[duoMfaOption] == "Passcode" {
		//get users DUO MFA Token
		token = prompter.StringRequired("Enter passcode")
	}

	// send mfa auth request
	duoSubmitURL = fmt.Sprintf("https://%s/frame/prompt", duoHost)

	duoForm = url.Values{}
	duoForm.Add("sid", duoSID)
	duoForm.Add("device", "phone1")
	duoForm.Add("factor", duoMfaOptions[duoMfaOption])
	duoForm.Add("out_of_date", "false")
	if duoMfaOptions[duoMfaOption] == "Passcode" {
		duoForm.Add("passcode", token)
	}

	req, err = http.NewRequest("POST", duoSubmitURL, strings.NewReader(duoForm.Encode()))
	if err != nil {
		return "", errors.Wrap(err, "error building authentication request")
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err = oc.client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "error retrieving verify response")
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrap(err, "error retrieving body from response")
	}

	resp := string(body)

	duoTxStat := gjson.Get(resp, "stat").String()
	duoTxID := gjson.Get(resp, "response.txid").String()
	if duoTxStat != "OK" {
		return "", errors.Wrap(err, "error authenticating mfa device")
	}

	// get duo cookie
	duoSubmitURL = fmt.Sprintf("https://%s/frame/status", duoHost)

	duoForm = url.Values{}
	duoForm.Add("sid", duoSID)
	duoForm.Add("txid", duoTxID)

	req, err = http.NewRequest("POST", duoSubmitURL, strings.NewReader(duoForm.Encode()))
	if err != nil {
		return "", errors.Wrap(err, "error building authentication request")
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err = oc.client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "error retrieving verify response")
	}

	body, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrap(err, "error retrieving body from response")
	}

	resp = string(body)

	duoTxResult := gjson.Get(resp, "response.result").String()
	duoResultURL := gjson.Get(resp, "response.result_url").String()

	fmt.Println(gjson.Get(resp, "response.status").String())

	if duoTxResult != "SUCCESS" {
		//poll as this is likely a push request
		for {
			time.Sleep(3 * time.Second)

			req, err = http.NewRequest("POST", duoSubmitURL, strings.NewReader(duoForm.Encode()))
			if err != nil {
				return "", errors.Wrap(err, "error building authentication request")
			}

			req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

			res, err = oc.client.Do(req)
			if err != nil {
				return "", errors.Wrap(err, "error retrieving verify response")
			}

			body, err = ioutil.ReadAll(res.Body)
			if err != nil {
				return "", errors.Wrap(err, "error retrieving body from response")
			}

			resp := string(body)

			duoTxResult = gjson.Get(resp, "response.result").String()
			duoResultURL = gjson.Get(resp, "response.result_url").String()

			fmt.Println(gjson.Get(resp, "response.status").String())

			if duoTxResult == "FAILURE" {
				return "", errors.Wrap(err, "failed to authenticate device")
			}

			if duoTxResult == "SUCCESS" {
				break
			}
		}
	}

	duoRequestURL := fmt.Sprintf("https://%s%s", duoHost, duoResultURL)
	req, err = http.NewRequest("POST", duoRequestURL, strings.NewReader(duoForm.Encode()))
	if err != nil {
		return "", errors.Wrap(err, "error constructing request object to result url")
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err = oc.client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "error retrieving duo result response")
	}

	body, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrap(err, "duoResultSubmit: error retrieving body from response")
	}

	resp = string(body)

	duoTxCookie := gjson.Get(resp, "response.cookie").String()
	if duoTxCookie == "" {
		return "", errors.Wrap(err, "duoResultSubmit: Unable to get response.cookie")
	}

	return duoTxCookie, nil
}

func parseTokens(blob string) (string, string, string, string) {
	hostRgx := regexp.MustCompile(`data-host=\"(.*?)\"`)
	sigRgx := regexp.MustCompile(`data-sig-request=\"(.*?)\"`)
	dpaRgx := regexp.MustCompile(`data-post-action=\"(.*?)\"`)

	dataSigRequest := sigRgx.FindStringSubmatch(blob)
	duoHost := hostRgx.FindStringSubmatch(blob)
	postAction := dpaRgx.FindStringSubmatch(blob)

	duoSignatures := strings.Split(dataSigRequest[1], ":")
	return duoHost[1], postAction[1], duoSignatures[0], duoSignatures[1]
}

func extractSamlResponse(res *http.Response) (string, error) {

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrap(err, "extractSamlResponse: error retrieving body from response")
	}

	bodyString := string(body)
	return "", errors.Wrap(err, bodyString)

	samlRgx := regexp.MustCompile(`name=\"SAMLResponse\" value=\"(.*?)\"/>`)
	samlResponseValue := samlRgx.FindStringSubmatch(string(body))
	return samlResponseValue[1], nil
}
