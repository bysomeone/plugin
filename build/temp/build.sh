#!/usr/bin/env bash


#declare -a arr=("docker" "para")
declare -a arr=("para")

chain33dir="/home/jiangpeng/go/src/github.com/33cn/chain33"
srcdir="/home/jiangpeng/go/src/github.com/33cn/plugin"
testdir="/home/jiangpeng/go/src/test"



cd $chain33dir && make clean && make && cd -

cp $chain33dir/build/chain33 ./
#cp $chain33dir/build/chain33-cli /usr/local/bin/33cli
cp $chain33dir/build/chain33 ../temp1
cp $chain33dir/build/chain33 ../temp2
