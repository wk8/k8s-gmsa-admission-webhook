package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func main() {
	logrus.SetOutput(os.Stdout)
	// TODO wkpo higher log level for the release image, through env var
	logrus.SetLevel(logrus.DebugLevel)

	server := http.Server{
		Addr: ":443",
	}

	http.HandleFunc("/validate-mutate", handleValidateAndMutate)

	if err := server.ListenAndServeTLS(env("TLS_CRT"), env("TLS_KEY")); err != nil {
		panic(err)
	}
}

func env(key string) string {
	if value, found := os.LookupEnv(key); found {
		return value
	}
	panic(fmt.Errorf("%s env var not found", key))
}

type podAdmissionError struct {
	error
	code int
	pod  *corev1.Pod
}

// toAdmissionResponse is a helper function to create an AdmissionResponse
// with an embedded error
func errorToAdmissionResponse(err error, httpCode ...int) *admissionv1beta1.AdmissionResponse {
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

func handleValidateAndMutate(responseWriter http.ResponseWriter, request *http.Request) {
	admissionResponse := validateAndMutateHttpRequestToAdmissionResponse(request)

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

func validateAndMutateHttpRequestToAdmissionResponse(request *http.Request) *admissionv1beta1.AdmissionResponse {
	// verify the content type is accurate
	contentType := request.Header.Get("Content-Type")
	if contentType != "application/json" {
		return errorToAdmissionResponse(fmt.Errorf("expected JSON content-type header"), 415)
	}

	// read the body
	if request.Body == nil {
		errorToAdmissionResponse(fmt.Errorf("no request body"), 400)
	}
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		return errorToAdmissionResponse(fmt.Errorf("couldn't read request body: %v", err), 400)
	}

	logrus.Debugf("handling request: %s", body)

	// unmarshall the request
	admissionReview := admissionv1beta1.AdmissionReview{}
	if err = json.Unmarshal(body, &admissionReview); err != nil {
		return errorToAdmissionResponse(fmt.Errorf("unable to unmarshall JSON body as an admission review: %v", err), 400)
	}
	if admissionReview.Request == nil {
		return errorToAdmissionResponse(fmt.Errorf("no 'Request' field in JSON body"), 400)
	}

	admissionResponse, admissionError := validateAndMutate(admissionReview.Request)
	if admissionError != nil {
		admissionResponse = errorToAdmissionResponse(admissionError)
	}

	// return the same UID
	admissionResponse.UID = admissionReview.Request.UID

	return admissionResponse
}

// TODO wkpo separate package?
func validateAndMutate(request *admissionv1beta1.AdmissionRequest) (*admissionv1beta1.AdmissionResponse, *podAdmissionError) {
	if request.Kind.Kind != "Pod" {
		return nil, &podAdmissionError{error: fmt.Errorf("expected a pod object, got a %v", request.Kind.Kind), code: 400}
	}

	pod := corev1.Pod{}
	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return nil, &podAdmissionError{error: fmt.Errorf("unable to unmarshall pod JSON object: %v", err), code: 400}
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
		return nil, &podAdmissionError{error: fmt.Errorf("unable to marshall patch JSON %v: %v", patchMap, err), code: 500}
	}
	logrus.Debugf("wkpo bordel!! %v => %v", pod.GenerateName, strings.Contains(pod.GenerateName, "denyme"))

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
