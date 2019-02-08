.DEFAULT_GOAL := wkpo # TODO wkpo
SHELL := /bin/bash

### Overridable env vars ###
KUBERNETES_VERSION ?= 1.13
# see https://github.com/kubernetes-sigs/kubeadm-dind-cluster/releases
KUBEADM_DIND_VERSION = v0.1.0
# path to glide, will be downloaded if needed
GLIDE_BIN ?= $(shell which glide 2> /dev/null)


### Sanity checks
ifeq ($(filter $(KUBERNETES_VERSION),1.11 1.12 1.13),)
$(error "Kubernetes version $(KUBERNETES_VERSION) not supported")
endif

ifeq ($(GLIDE_BIN),)
ifeq ($(GOPATH),)
# TODO wkpo: shouldn't require this
$(error '$$GOPATH not defined, cannot install glide')
endif
GLIDE_BIN = $(GOPATH)/bin/glide
endif


### Internals variables
GO_VERSION = 1.11.5

DOCKER_BUILD = docker build . --build-arg GO_VERSION=$(GO_VERSION)

# kubeadm DIND settings
KUBEADM_DIND_CLUSTER_SCRIPT = kubeadm_dind_scripts/$(KUBEADM_DIND_VERSION)/dind-cluster-v$(KUBERNETES_VERSION).sh
KUBEADM_DIND_CLUSTER_SCRIPT_URL = https://github.com/kubernetes-sigs/kubeadm-dind-cluster/releases/download/$(KUBEADM_DIND_VERSION)/dind-cluster-v$(KUBERNETES_VERSION).sh
KUBEADM_DIND_DIR = ~/.kubeadm-dind-cluster
ADMISSION_PLUGINS = NodeRestriction,MutatingAdmissionWebhook,ValidatingAdmissionWebhook

DEV_IMAGE_NAME = k8s-gmsa-webhook-dev
DEPLOYMENT_NAME = k8s-gmsa-admission-webhook
NAMESPACE = kube-system
KUBECTL = $(KUBEADM_DIND_DIR)/kubectl
KUBECTLNS = $(KUBECTL) --namespace=$(NAMESPACE)
TLS_DIR = deploy/tls


# starts a new DIND cluster (see https://github.com/kubernetes-sigs/kubeadm-dind-cluster)
.PHONY: start_cluster
start_cluster: $(KUBEADM_DIND_CLUSTER_SCRIPT)
	NUM_NODES=1 APISERVER_enable_admission_plugins=$(ADMISSION_PLUGINS) $(KUBEADM_DIND_CLUSTER_SCRIPT) up

# stops the DIND cluster
.PHONY: stop_cluster
stop_cluster: $(KUBEADM_DIND_CLUSTER_SCRIPT)
	$(KUBEADM_DIND_CLUSTER_SCRIPT) down

# removes the DIND cluster
.PHONY: clean_cluster
clean_cluster: clean_ssl stop_cluster
	$(KUBEADM_DIND_CLUSTER_SCRIPT) clean
	rm -rf $(KUBEADM_DIND_DIR)

# starts the DIND cluster only if it's not already running
.PHONY: start_cluster_if_not_running
start_cluster_if_not_running: $(KUBEADM_DIND_CLUSTER_SCRIPT)
	@ if [ -x $(KUBECTL) ] && timeout 2 $(KUBECTL) version &> /dev/null; then \
		echo "Dev cluster already running"; \
	else \
		$(MAKE) start_cluster; \
	fi

# deploys the webhook to the DIND cluster with the dev image
.PHONY: deploy_dev_webhook
deploy_dev_webhook:
	K8S_GMSA_IMAGE=$(DEV_IMAGE_NAME) $(MAKE) _deploy_webhook

# deploys the webhook to the DIND cluster
.PHONY: _deploy_webhook
_deploy_webhook: _copy_image_if_needed $(TLS_DIR)/server-key.pem $(TLS_DIR)/server-cert.pem remove_webhook
	@ [ "$$K8S_GMSA_IMAGE" ]
	@ TLS_PRIVATE_KEY=$$(cat "$(TLS_DIR)/server-key.pem" | base64 -w 0) \
		TLS_CERTIFICATE=$$(cat "$(TLS_DIR)/server-cert.pem" | base64 -w 0) \
		CA_BUNDLE=$$($(KUBECTL) get configmap -n kube-system extension-apiserver-authentication -o=jsonpath='{.data.client-ca-file}' | base64 -w 0) \
		DEPLOYMENT_NAME=$(DEPLOYMENT_NAME) \
		IMAGE_NAME="$$K8S_GMSA_IMAGE" \
		NAMESPACE=$(NAMESPACE) \
			envsubst < deploy/gmsa-webhook.yml.tpl | $(KUBECTL) apply -f -

# copies the image to the DIND cluster only if it's not already up-to-date
.PHONY: _copy_image_if_needed
_copy_image_if_needed: start_cluster_if_not_running
	@ [ "$$K8S_GMSA_IMAGE" ]
	@ LOCAL_IMG_ID=$$(docker image inspect "$$K8S_GMSA_IMAGE" -f '{{ .Id }}'); \
	STATUS=$$? ; if [[ $$STATUS != 0 ]]; then echo "Unable to retrieve image ID for $$K8S_GMSA_IMAGE"; exit $$STATUS; fi; \
	REMOTE_IMG_ID=$$(docker exec kube-master docker image inspect "$$K8S_GMSA_IMAGE" -f '{{ .Id }}' 2> /dev/null); \
	if [[ $$? == 0 ]] && [[ "$$REMOTE_IMG_ID" == "$$LOCAL_IMG_ID" ]]; then \
		echo "Image $$K8S_GMSA_IMAGE already up-to-date in DIND cluster"; \
	else \
		echo "Copying image $$K8S_GMSA_IMAGE to DIND cluster..."; \
		$(KUBEADM_DIND_CLUSTER_SCRIPT) copy-image "$$K8S_GMSA_IMAGE"; \
	fi

$(TLS_DIR)/%.pem:
	@ mkdir -p $(TLS_DIR)
	./deploy/create-signed-cert.sh --service $(DEPLOYMENT_NAME) --namespace $(NAMESPACE) --tmp-dir $(TLS_DIR)

.PHONY: clean_ssl
clean_ssl:
	rm -rf $(TLS_DIR)

# removes the webhook from the cluster
.PHONY: remove_webhook
remove_webhook:
	@ if $(KUBECTLNS) get validatingwebhookconfigurations $(DEPLOYMENT_NAME) &> /dev/null; then $(KUBECTLNS) delete validatingwebhookconfigurations $(DEPLOYMENT_NAME); fi
	@ if $(KUBECTLNS) get service $(DEPLOYMENT_NAME) &> /dev/null; then $(KUBECTLNS) delete service $(DEPLOYMENT_NAME); fi
	@ if $(KUBECTLNS) get deployment $(DEPLOYMENT_NAME) &> /dev/null; then $(KUBECTLNS) delete deployment $(DEPLOYMENT_NAME); fi
	@ if $(KUBECTLNS) get secret $(DEPLOYMENT_NAME) &> /dev/null; then $(KUBECTLNS) delete secret $(DEPLOYMENT_NAME); fi

# downloads kubeadm-dind scripts
$(KUBEADM_DIND_CLUSTER_SCRIPT):
	mkdir -p $(dir $(KUBEADM_DIND_CLUSTER_SCRIPT))
	if which curl &> /dev/null; then \
		curl -L $(KUBEADM_DIND_CLUSTER_SCRIPT_URL) > $(KUBEADM_DIND_CLUSTER_SCRIPT); \
	else \
		wget -O $(KUBEADM_DIND_CLUSTER_SCRIPT) $(KUBEADM_DIND_CLUSTER_SCRIPT_URL) ; \
	fi
	chmod +x $(KUBEADM_DIND_CLUSTER_SCRIPT)

.PHONY: install_deps
install_deps: $(GLIDE_BIN)
	$(GLIDE_BIN) install -v

GLIDE_URL = https://glide.sh/get
$(GLIDE_BIN):
	mkdir -p $(dir $(GLIDE_BIN))
	if which curl &> /dev/null; then \
		curl $(GLIDE_URL) | sh; \
	else \
		wget -O - $(GLIDE_URL) 2> /dev/null | sh; \
	fi

.PHONY: build_dev_image
build_dev_image:
	$(DOCKER_BUILD) -f Dockerfile.dev -t $(DEV_IMAGE_NAME)

.PHONY:
clean: clean_cluster clean_sync


## Ksync (https://github.com/vapor-ware/ksync) is a pretty nice way to keep the dev container
## in sync with the local repo
## It won't work as is though... see https://github.com/vapor-ware/ksync/pull/264
## If you want to use my fork in the meantime just clone https://github.com/wk8/ksync, and then
## make build-cmd && cp bin/ksync $(which ksync)

KSYNC = ksync --namespace $(NAMESPACE)
KSYNC_DAEMON_PID_FILE = ~/.ksync/daemon.pid
KSYNC_NAME = k8s-gmsa-admission-webhook

.PHONY: start_sync
start_sync: _init_ksync_if_needed
	@ if $(KSYNC) get $(DEPLOYMENT_NAME) | grep $(DEPLOYMENT_NAME) > /dev/null; then \
		echo "ksync already started"; \
	else \
		$(KSYNC) create --selector=app=$(DEPLOYMENT_NAME) $(CURDIR) /go/src/github.com/wk8/k8s-gmsa-admission-webhook --name $(KSYNC_NAME) --reload=false; \
	fi

.PHONY: stop_sync
stop_sync:
	@ if $(KSYNC) get $(DEPLOYMENT_NAME) | grep $(DEPLOYMENT_NAME) > /dev/null; then \
		$(KSYNC) delete $(DEPLOYMENT_NAME); \
	else \
		echo "ksync not started"; \
	fi

.PHONY: clean_sync
clean_sync: stop_sync
	$(KSYNC) clean --nuke --local --remote

.PHONY: _init_ksync_if_needed
_init_ksync_if_needed: _install_ksync_if_needed
	@ if ! kill -0 $$(cat $(KSYNC_DAEMON_PID_FILE) 2> /dev/null) &> /dev/null; then \
		$(KSYNC) init --docker-root /dind/docker --docker-socket /run/docker.sock \
			&& $(KSYNC) watch --daemon; \
	fi

KSYNC_URL = https://vapor-ware.github.io/gimme-that/gimme.sh
.PHONY: _install_ksync_if_needed
_install_ksync_if_needed:
	@ if ! which ksync &> /dev/null; then \
		echo "Installing ksync"; \
		if which curl &> /dev/null; then \
			curl $(KSYNC_URL) | sh; \
		else \
			wget -O - $(KSYNC_URL) 2> /dev/null | sh; \
		fi ; \
	fi