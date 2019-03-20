#!/bin/sh
# proto生成命令，将pb.go文件生成到types目录下
protoc --go_out=plugins=grpc:../types/calculator ./*.proto
