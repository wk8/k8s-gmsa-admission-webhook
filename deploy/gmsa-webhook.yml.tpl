## Template to deploy the GMSA webhook
## TODO: make this a helmchart instead?

apiVersion: v1
kind: Secret
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
data:
  tls_private_key: ${TLS_PRIVATE_KEY}
  tls_certificate: ${TLS_CERTIFICATE}

---

apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${DEPLOYMENT_NAME}
  template:
    metadata:
      labels:
        app: ${DEPLOYMENT_NAME}
    spec:
      containers:
      - name: ${DEPLOYMENT_NAME}
        image: ${IMAGE_NAME}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 443
        volumeMounts:
          - name: tls
            mountPath: "/tls"
            readOnly: true
        env:
          - name: TLS_KEY
            value: /tls/key
          - name: TLS_CRT
            value: /tls/crt
      volumes:
      - name: tls
        secret:
          secretName: ${DEPLOYMENT_NAME}
          items:
          - key: tls_private_key
            path: key
          - key: tls_certificate
            path: crt

---

apiVersion: v1
kind: Service
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
spec:
  ports:
  - port: 443
    targetPort: 443
  selector:
    app: ${DEPLOYMENT_NAME}

---

## TODO wkpo next mutating?
#apiVersion: admissionregistration.k8s.io/v1beta1
#kind: ValidatingWebhookConfiguration
#metadata:
#  name: ${DEPLOYMENT_NAME}
#webhooks:
#- name: k8s-gmsa-admission-webhook.wk8.github.com
#  clientConfig:
#    service:
#      name: ${DEPLOYMENT_NAME}
#      namespace: ${NAMESPACE}
#      path: "/validate"
#    caBundle: ${CA_BUNDLE}
#  rules:
#  - operations: ["CREATE", "UPDATE"]
#    apiGroups: [""]
#    apiVersions: ["*"]
#    resources: ["pods"]
#  failurePolicy: Fail
#  # don't run on kube-system
#  namespaceSelector:
#    matchExpressions:
#      - key: Name
#        operator: NotIn
#        values: ["kube-system"]
