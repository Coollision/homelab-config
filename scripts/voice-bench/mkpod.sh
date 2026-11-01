#!/bin/bash
# usage: mkpod.sh name selector-json cpu-limit mem-limit [image]
name=$1; sel=$2; cpu=$3; mem=$4; img=${5:-rhasspy/wyoming-whisper:3.8.1}
kubectl -n smarthome run $name --image=$img --restart=Never --labels=bench=voice --overrides="{\"spec\":{\"nodeSelector\":$sel,\"containers\":[{\"name\":\"$name\",\"image\":\"$img\",\"command\":[\"sleep\",\"43200\"],\"resources\":{\"requests\":{\"cpu\":\"100m\",\"memory\":\"256Mi\"},\"limits\":{\"cpu\":\"$cpu\",\"memory\":\"$mem\"}}}]}}" >/dev/null
kubectl -n smarthome wait pod/$name --for=condition=Ready --timeout=300s
