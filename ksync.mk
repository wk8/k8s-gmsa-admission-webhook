## Ksync (https://github.com/vapor-ware/ksync) is a pretty nice way to keep the dev container
## in sync with the local repo
## It won't work as is though... see https://github.com/vapor-ware/ksync/pull/264
## If you want to use my fork in the meantime just clone https://github.com/wk8/ksync, and then
## make build-cmd && cp bin/ksync $(which ksync)

KSYNC = ksync --namespace $(NAMESPACE)
KSYNC_DIR = ~/.ksync
KSYNC_DAEMON_PID_FILE = $(KSYNC_DIR)/daemon.pid
KSYNC_NAME = k8s-gmsa-admission-webhook

.PHONY: start_sync
start_sync: _init_ksync_if_needed
	@ if $(KSYNC) get $(KSYNC_NAME) | grep $(KSYNC_NAME) > /dev/null; then \
		echo "ksync already started"; \
	else \
		$(KSYNC) create --selector=app=$(DEPLOYMENT_NAME) $(CURDIR) /go/src/github.com/wk8/k8s-gmsa-admission-webhook --name $(KSYNC_NAME) --reload=false; \
	fi

.PHONY: stop_sync
stop_sync:
	@ if $(KSYNC) get $(KSYNC_NAME) | grep $(KSYNC_NAME) > /dev/null || grep $(KSYNC_NAME) $(KSYNC_DIR)/ksync.yaml &> /dev/null; then \
		$(KSYNC) delete $(KSYNC_NAME); \
	fi
	@ rm -f $(KSYNC_DAEMON_PID_FILE)
	@ if $(KUBECTLNS) get daemonset ksync &> /dev/null; then $(KUBECTLNS) delete daemonset ksync; fi
	@ if [ -x $(KUBECTL) ]; then $(KUBECTLNS) delete pod --selector=app=ksync > /dev/null; fi

.PHONY: restart_sync
restart_sync: stop_sync start_sync

.PHONY: clean_sync
clean_sync: stop_sync
	$(KSYNC) clean --nuke --local --remote || true

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
