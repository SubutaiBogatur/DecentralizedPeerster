#!/usr/bin/env bash

# tests work only with HW1

go build
cd client
go build
cd ..

RED='\033[0;31m'
NC='\033[0m'
DEBUG="false"

outputFiles=()
message=Weather_is_clear
message2=Winter_is_coming


UIPort=12345
gossipPort=5000
name='A'

# General peerster (gossiper) command
#./Peerster -UIPort=12345 -gossipPort=127.0.0.1:5001 -name=A -peers=127.0.0.1:5002 > A.out &

for i in `seq 1 10`;
do
	outFileName="$name.out"
	peerPort=$((($gossipPort+1)%10+5000))
	peer="127.0.0.1:$peerPort"
	gossipAddr="127.0.0.1:$gossipPort"
	./Peerster -UIPort=$UIPort -gossipAddr=$gossipAddr -name=$name -simple -peers=$peer > $outFileName &
	outputFiles+=("$outFileName")
	if [[ "$DEBUG" == "true" ]] ; then
		echo "$name running at UIPort $UIPort and gossipPort $gossipPort"
	fi
	UIPort=$(($UIPort+1))
	gossipPort=$(($gossipPort+1))
	name=$(echo "$name" | tr "A-Y" "B-Z")
done

./client/client -UIPort=12349 -msg=$message
./client/client -UIPort=12346 -msg=$message2
sleep 3
pkill -f Peerster


sleep 1
echo " --- starting testing --- "
sleep 5


#testing
failed="F"

if !(grep -q "CLIENT MESSAGE $message" "E.out") ; then
        echo "lox1"
	failed="T"
fi

if !(grep -q "CLIENT MESSAGE $message2" "B.out") ; then
  echo "lox2"
  failed="T"
fi

if [[ "$failed" == "T" ]] ; then
	echo -e "${RED}FAILED${NC}"
fi

echo "From the first sight you're not a lox"
# echo "${outputFiles[@]}"

gossipPort=5000
for i in `seq 0 9`;
do
	relayPort=$(($gossipPort-1))
	if [[ "$relayPort" == 4999 ]] ; then
		relayPort=5009
	fi
	nextPort=$((($gossipPort+1)%10+5000))
	msgLine="SIMPLE MESSAGE origin E from 127.0.0.1:$relayPort contents $message"
	msgLine2="SIMPLE MESSAGE origin B from 127.0.0.1:$relayPort contents $message2"
	peersLine="127.0.0.1:$nextPort,127.0.0.1:$relayPort"
	if [[ "$DEBUG" == "true" ]] ; then
		echo "check 1 $msgLine"
		echo "check 2 $msgLine2"
		echo "check 3 $peersLine"
	fi
	gossipPort=$(($gossipPort+1))
	if !(grep -q "$msgLine" "${outputFiles[$i]}") ; then
                echo "lox11"
   		failed="T"
	fi
	if !(grep -q "$peersLine" "${outputFiles[$i]}") ; then
        echo "lox12"
        failed="T"
    fi
	if !(grep -q "$msgLine2" "${outputFiles[$i]}") ; then
        echo "lox13"
        failed="T"
    fi
done

if [[ "$failed" == "T" ]] ; then
    echo -e "${RED}***FAILED***${NC}"
else
        echo "From the second sight you're also not a lox, my respekt"
	echo "***PASSED***"
fi



#sleep 2
