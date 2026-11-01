#!/bin/bash
# usage: mkcpp.sh name nodeName image cpu mem dri(0|1)
name=$1; node=$2; img=$3; cpu=$4; mem=$5; dri=$6
vm='[]'; vol='[{"name":"m","emptyDir":{}}]'; mnt='[{"name":"m","mountPath":"/models"}]'; sc='{}'
if [ "$dri" = 1 ]; then vol='[{"name":"m","emptyDir":{}},{"name":"dri","hostPath":{"path":"/dev/dri"}}]'; mnt='[{"name":"m","mountPath":"/models"},{"name":"dri","mountPath":"/dev/dri"}]'; sc='{"privileged":true}'; fi
kubectl -n smarthome apply -f - >/dev/null <<EOT
{"apiVersion":"v1","kind":"Pod","metadata":{"name":"$name","namespace":"smarthome","labels":{"bench":"voice"}},
 "spec":{"nodeSelector":{"kubernetes.io/hostname":"$node"},"containers":[{"name":"c","image":"$img","command":["sleep","86400"],"securityContext":$sc,"volumeMounts":$mnt,
 "resources":{"requests":{"cpu":"100m","memory":"256Mi"},"limits":{"cpu":"$cpu","memory":"$mem"}}}],"volumes":$vol}}
EOT
kubectl -n smarthome wait pod/$name --for=condition=Ready --timeout=240s | tail -1
kubectl -n smarthome exec $name -- mkdir -p /audio
tar cf - -C audio . | kubectl -n smarthome exec -i $name -- tar xf - -C /audio
kubectl -n smarthome exec -i $name -- sh -c 'cat > /wcpp.sh; chmod +x /wcpp.sh' < wcpp.sh
