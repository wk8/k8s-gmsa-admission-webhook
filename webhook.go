package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type webhook struct {
	server *http.Server
	client *kubeClient
}

func newWebhook(client *kubeClient) *webhook {
	return &webhook{
		client: client,
	}
}

// start is a blocking call.
func (webhook *webhook) start(port int, tlsConfig *tlsConfig) error {
	if webhook.server != nil {
		return fmt.Errorf("webhook already started")
	}

	webhook.server = &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: webhook,
	}

	logrus.Debugf("starting webhook server at port %v", port)
	var err error
	if tlsConfig == nil {
		err = webhook.server.ListenAndServe()
	} else {
		err = webhook.server.ListenAndServeTLS(tlsConfig.crtPath, tlsConfig.keyPath)
	}

	if err != nil {
		if err == http.ErrServerClosed {
			logrus.Debugf("server closed")
		} else {
			return err
		}
	}

	return nil
}

func (webhook *webhook) stop() error {
	if webhook.server == nil {
		return fmt.Errorf("webhook server not started yet")
	}
	return webhook.server.Shutdown(context.Background())
}

func (webhook *webhook) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	// only one endpoint, no need for a router here
	if request.URL.Path != "/validate-mutate" {
		logrus.Infof("received request for unknown path %s", request.URL.Path)
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	admissionResponse := webhook.httpRequestToAdmissionResponse(request)

	responseAdmissionReview := admissionv1beta1.AdmissionReview{Response: admissionResponse}
	if responseBytes, err := json.Marshal(responseAdmissionReview); err == nil {
		logrus.Debugf("sending response: %s", responseBytes)

		if _, err = responseWriter.Write(responseBytes); err != nil {
			logrus.Errorf("error when writing response JSON %s: %v", responseBytes, err)
		}
	} else {
		logrus.Errorf("error when marshalling response %v: %v", responseAdmissionReview, err)
	}
}

func (webhook *webhook) httpRequestToAdmissionResponse(request *http.Request) *admissionv1beta1.AdmissionResponse {
	// verify the content type is accurate
	contentType := request.Header.Get("Content-Type")
	if contentType != "application/json" {
		return deniedAdmissionResponse(fmt.Errorf("expected JSON content-type header"), http.StatusUnsupportedMediaType)
	}

	// read the body
	if request.Body == nil {
		deniedAdmissionResponse(fmt.Errorf("no request body"), http.StatusBadRequest)
	}
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		return deniedAdmissionResponse(fmt.Errorf("couldn't read request body: %v", err), http.StatusBadRequest)
	}

	logrus.Debugf("handling request: %s", body)

	// unmarshall the request
	admissionReview := admissionv1beta1.AdmissionReview{}
	if err = json.Unmarshal(body, &admissionReview); err != nil {
		return deniedAdmissionResponse(fmt.Errorf("unable to unmarshall JSON body as an admission review: %v", err), http.StatusBadRequest)
	}
	if admissionReview.Request == nil {
		return deniedAdmissionResponse(fmt.Errorf("no 'Request' field in JSON body"), http.StatusBadRequest)
	}

	admissionResponse, admissionError := webhook.validateAndMutate(admissionReview.Request)
	if admissionError != nil {
		admissionResponse = deniedAdmissionResponse(admissionError)
	}

	// return the same UID
	admissionResponse.UID = admissionReview.Request.UID

	return admissionResponse
}

func (webhook *webhook) validateAndMutate(request *admissionv1beta1.AdmissionRequest) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	if request.Kind.Kind != "Pod" {
		return nil, &podAdmissionError{error: fmt.Errorf("expected a pod object, got a %v", request.Kind.Kind), code: http.StatusBadRequest}
	}

	pod := corev1.Pod{}
	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return nil, &podAdmissionError{error: fmt.Errorf("unable to unmarshall pod JSON object: %v", err), code: http.StatusBadRequest}
	}

	patchMap := []map[string]string{
		{
			"op":    "add",
			"path":  "/metadata/annotations/wkpo_annotation",
			"value": "coucou",
		},
		{
			"op":    "add",
			"path":  "/metadata/annotations/wkpo_annotation_2",
			"value": "coucou_po",
		},
	}
	patchBytes, err := json.Marshal(patchMap)
	if err != nil {
		return nil, &podAdmissionError{error: fmt.Errorf("unable to marshall patch JSON %v: %v", patchMap, err), code: http.StatusInternalServerError}
	}

	patchType := admissionv1beta1.PatchTypeJSONPatch
	admissionResponse := &admissionv1beta1.AdmissionResponse{
		Allowed: !strings.Contains(pod.Name, "denyme") && !strings.Contains(pod.GenerateName, "denyme"),
		Result: &metav1.Status{
			Message: "you asked to be denied",
		},
		Patch:     patchBytes,
		PatchType: &patchType,
	}

	return admissionResponse, nil
}

// deniedAdmissionResponse is a helper function to create an AdmissionResponse
// with an embedded error
func deniedAdmissionResponse(err error, httpCode ...int) *admissionv1beta1.AdmissionResponse {
	var code int
	logMsg := "refusing to admit"

	if admissionError, ok := err.(podAdmissionError); ok {
		code = admissionError.code
		if admissionError.pod != nil {
			logMsg += fmt.Sprintf(" pod %+v", admissionError.pod)
		}
	}

	if len(httpCode) > 0 {
		code = httpCode[0]
	}

	if code != 0 {
		logMsg += fmt.Sprintf(" with code %v", code)
	}

	logrus.Infof(logMsg+": %v", err.Error())

	return &admissionv1beta1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: err.Error(),
			Code:    int32(code),
		},
	}
}

type podAdmissionError struct {
	error
	code int
	pod  *corev1.Pod
}
