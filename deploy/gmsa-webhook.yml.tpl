## Template to deploy the GMSA webhook
## TODO: make this a helmchart instead?

# add a label to the deployment's namespace so that we can exclude it
apiVersion: v1
kind: Namespace
metadata:
  name: ${NAMESPACE}
  labels:
    gmsa-webhook: disabled

---

apiVersion: v1
kind: Secret
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
data:
  tls_private_key: ${TLS_PRIVATE_KEY}
  tls_certificate: ${TLS_CERTIFICATE}

---

# the service account for the webhook
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}

---

# create an RBAC role to allow reading GMSA cred specs
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: gmsa-cred-spec-reader
rules:
- apiGroups: ["windows.k8s.io"]
  resources: ["gmsacredentialspecs"]
  verbs: ["get"]

---

# and bind it to the webhook's service account
kind: ClusterRoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: allow-gmsa-webhook-to-read-cred-specs
  namespace: ${NAMESPACE}
subjects:
- kind: ServiceAccount
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
roleRef:
  kind: ClusterRole
  name: gmsa-cred-spec-reader
  apiGroup: rbac.authorization.k8s.io

---

## create an RBAC role to allow creating access reviews (ie checking authz)
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: localsubjectaccessreview-creator
rules:
- apiGroups: ["authorization.k8s.io"]
  resources: ["localsubjectaccessreviews"]
  verbs: ["create"]

---

# and bind it to the webhook's service account
kind: ClusterRoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: allow-gmsa-webhook-to-create-localsubjectaccessreview
  namespace: ${NAMESPACE}
subjects:
- kind: ServiceAccount
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
roleRef:
  kind: ClusterRole
  name: localsubjectaccessreview-creator
  apiGroup: rbac.authorization.k8s.io

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
      serviceAccountName: ${DEPLOYMENT_NAME}
      nodeSelector:
        beta.kubernetes.io/os: linux
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

# declare the CRD to be used
apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: gmsacredentialspecs.windows.k8s.io
spec:
  group: windows.k8s.io
  version: v1alpha1
  names:
    kind: GMSACredentialSpec
    plural: gmsacredentialspecs
  scope: Cluster
  validation:
    openAPIV3Schema:
      properties:
        credspec:
          description: GMSA Credential Spec
          type: object

---

apiVersion: admissionregistration.k8s.io/v1beta1
kind: ValidatingWebhookConfiguration
metadata:
  name: ${DEPLOYMENT_NAME}
webhooks:
- name: k8s-gmsa-admission-webhook.wk8.github.com
  clientConfig:
    service:
      name: ${DEPLOYMENT_NAME}
      namespace: ${NAMESPACE}
      path: "/validate"
    caBundle: ${CA_BUNDLE}
  rules:
  - operations: ["CREATE", "UPDATE"]
    apiGroups: [""]
    apiVersions: ["*"]
    resources: ["pods"]
  failurePolicy: Fail
  # don't run on ${NAMESPACE}
  namespaceSelector:
    matchExpressions:
      - key: gmsa-webhook
        operator: NotIn
        values: [disabled]

---

apiVersion: admissionregistration.k8s.io/v1beta1
kind: MutatingWebhookConfiguration
metadata:
  name: ${DEPLOYMENT_NAME}
webhooks:
- name: k8s-gmsa-admission-webhook.wk8.github.com
  clientConfig:
    service:
      name: ${DEPLOYMENT_NAME}
      namespace: ${NAMESPACE}
      path: "/mutate"
    caBundle: ${CA_BUNDLE}
  rules:
  - operations: ["CREATE"]
    apiGroups: [""]
    apiVersions: ["*"]
    resources: ["pods"]
  failurePolicy: Fail
  # don't run on ${NAMESPACE}
  namespaceSelector:
    matchExpressions:
    - key: gmsa-webhook
      operator: NotIn
      values: [disabled]
