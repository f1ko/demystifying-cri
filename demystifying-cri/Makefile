CLUSTER_NAME?=demystifying-cri
BINARY_NAME?=demystifying-cri
BINARY_PATH?=/opt/${BINARY_NAME}
SOCKET?=/var/run/demystifying-cri.sock

.PHONY: cluster create init setup start
cluster create init setup start:
	kind create cluster --name ${CLUSTER_NAME}
	docker exec demystifying-cri-control-plane sed -i 's#$KUBELET_EXTRA_ARGS#$KUBELET_EXTRA_ARGS --container-runtime-endpoint=unix:///var/run/demystifying-cri.sock#' /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
	docker exec demystifying-cri-control-plane systemctl daemon-reload
	docker exec demystifying-cri-control-plane apt update
	docker exec demystifying-cri-control-plane apt install -y skopeo
	docker exec demystifying-cri-control-plane apt install -y umoci

.PHONY: delete destroy stop
delete destroy stop:
	kind delete cluster --name ${CLUSTER_NAME}

.PHONY: build
build:
	GOOS=linux GOARCH=arm go build -o ${BINARY_NAME} demystifyingcri.go

.PHONY: copy
copy:
	docker cp ${BINARY_NAME} demystifying-cri-control-plane:${BINARY_PATH}

.PHONY: run
run: clean build copy
	docker exec demystifying-cri-control-plane systemctl restart kubelet
	docker exec demystifying-cri-control-plane ${BINARY_PATH}

.PHONY: test
test:
	kubectl run httpd --image httpd

.PHONY: clean clear
clean clear:
	- rm ${BINARY_NAME}
	- docker exec demystifying-cri-control-plane rm ${BINARY_PATH}
	- docker exec demystifying-cri-control-plane rm ${SOCKET}
