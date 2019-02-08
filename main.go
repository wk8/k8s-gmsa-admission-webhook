package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func main() {
	server := http.Server{
		Addr:    "0.0.0.0:443",
		Handler: &dummyServer{},
	}

	crtFile := env("TLS_CRT")
	keyFile := env("TLS_KEY")

	if err := server.ListenAndServeTLS(crtFile, keyFile); err != nil {
		panic(err)
	}
}

func env(key string) string {
	if value, found := os.LookupEnv(key); found {
		return value
	}
	panic(fmt.Errorf("%s env var not found", key))
}

type dummyServer struct{}

func (*dummyServer) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		panic(err)
	}

	fmt.Println("new request", request.Method, request.URL)
	fmt.Println("headers:", request.Header)
	fmt.Println("body:", string(body))

	requestedAdmissionReview := admissionv1beta1.AdmissionReview{}

	deserializer := codecs.UniversalDeserializer()
	_, _, err = deserializer.Decode(body, nil, &requestedAdmissionReview)
	if err != nil {
		fmt.Println("error when deserializing request: ", err)
		return
	}

	responseAdmissionReview := admissionv1beta1.AdmissionReview{
		Response: &admissionv1beta1.AdmissionResponse{
			Allowed: false,
			Result:  &metav1.Status{Message: "hello world"},
		},
	}

	if requestedAdmissionReview.Request != nil {
		responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID

		if requestedAdmissionReview.Request.Kind.Kind != "Pod" {
			fmt.Println("not a pod")
			return
		}
		pod := corev1.Pod{}
		if _, _, err = deserializer.Decode(requestedAdmissionReview.Request.Object.Raw, nil, &pod); err != nil {
			fmt.Println("error when deserializing pod: ", err)
			return
		}
		responseAdmissionReview.Response.Allowed = pod.Namespace == "kube-system"
	}

	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		panic(err)
	}
	if _, err := responseWriter.Write(respBytes); err != nil {
		panic(err)
	}
}
