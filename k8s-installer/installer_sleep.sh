#!/bin/bash

#This is dummy script as k8s currently don't allow only initContainers.
i="0"

while [ $i -eq 0 ]
do
	#wakeup once a day
	sleep 86400
done
