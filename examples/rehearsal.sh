#!/usr/bin/env bash
# A rehearsal of a pool across machines, on one machine: each node runs in
# a container of its own, with its own network address and its own disk,
# and the two workers sit on networks that cannot reach each other. It is
# as near to docs/multi-machine-test.md as one machine gets, and takes a
# couple of minutes. It needs Docker.
#
#   examples/rehearsal.sh            # run it, report, and clear up
#   KEEP=1 examples/rehearsal.sh     # leave the pool running afterwards
#   examples/rehearsal.sh down       # clear up a pool left running
#
#   MODEL=llama3.1:8b examples/rehearsal.sh
#       also have a worker serve the models of the Ollama on this machine
#       and have the client ask that model something through the pool. So
#       that it can reach the Ollama, that worker shares this machine's own
#       network instead of having one of its own.
set -euo pipefail
cd "$(dirname "$0")/.."

P=sisyphus-rehearsal
IMAGE=alpine:3.20
MODEL=${MODEL:-}

down() {
	docker rm -f $P-coordinator $P-north $P-south $P-laptop $P-stray >/dev/null 2>&1 || true
	docker network rm $P-a $P-b >/dev/null 2>&1 || true
}
if [ "${1:-}" = down ]; then
	down
	exit 0
fi

passed=0
failed=0
ok() {
	echo "  ok    $1"
	passed=$((passed + 1))
}
bad() {
	echo "  FAIL  $1"
	failed=$((failed + 1))
}
# check <what> <command...> passes if the command succeeds.
check() {
	local what=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}
# until_ok <seconds> <command...> waits for the command to succeed.
until_ok() {
	local left=$1
	shift
	until "$@" >/dev/null 2>&1; do
		left=$((left - 1))
		[ "$left" -gt 0 ] || return 1
		sleep 1
	done
}
# on <node> <sisyphusd arguments...> runs a command on a node.
on() {
	local node=$1
	shift
	docker exec "$P-$node" /sisyphus/sisyphusd "$@"
}

echo "building sisyphusd for the containers"
bin=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || { down; rm -rf "$bin"; }' EXIT
CGO_ENABLED=0 GOOS=linux go build -o "$bin/sisyphusd" ./apps/sisyphusd
cp examples/boulder.txt "$bin/"
chmod -R a+rX "$bin"

down
docker network create $P-a >/dev/null
docker network create $P-b >/dev/null
# start <node> <network> <sisyphusd run arguments...>
start() {
	local node=$1 network=$2
	shift 2
	docker run -d --name "$P-$node" --hostname "$node" --network "$network" --network-alias "$node" \
		--add-host host.docker.internal:host-gateway -v "$bin:/sisyphus:ro" "$IMAGE" \
		/sisyphus/sisyphusd run "$@" >/dev/null
}

echo "the coordinator, on both networks"
start coordinator $P-a --role coordinator --listen 0.0.0.0:7700 --name coordinator
docker network connect --alias coordinator $P-b $P-coordinator
until_ok 30 on coordinator nodes || { bad "the coordinator started"; docker logs $P-coordinator | tail -5; exit 1; }
ok "the coordinator started"

echo "two workers, on networks that cannot reach each other"
north_invitation=$(on coordinator pool invite | tail -1)
south_invitation=$(on coordinator pool invite | tail -1)
if [ -z "$MODEL" ]; then
	start north $P-a --role worker --coordinator coordinator:7700 --join "$north_invitation" --name north --slots 2
else
	# On this machine's own network, where the Ollama is, reaching the
	# coordinator by the address it has on the first network.
	at=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$P-a\").IPAddress}}" $P-coordinator)
	docker run -d --name $P-north --network host -v "$bin:/sisyphus:ro" "$IMAGE" \
		/sisyphus/sisyphusd run --role worker --coordinator "$at:7700" --join "$north_invitation" --name north --slots 2 \
		--data-dir /tmp/north --models-from http://127.0.0.1:11434 >/dev/null
fi
start south $P-b --role worker --coordinator coordinator:7700 --join "$south_invitation" --name south --slots 2
joined() { on coordinator nodes | grep -q north && on coordinator nodes | grep -q south; }
if until_ok 30 joined; then ok "both workers joined, each with its invitation"; else bad "both workers joined"; fi
if docker exec $P-south ping -c 1 -W 1 north >/dev/null 2>&1; then bad "the workers cannot reach each other directly (they can: the rehearsal is not testing what it says)"; else ok "the workers cannot reach each other directly"; fi

echo "an invitation is good once"
docker run -d --name $P-stray --network $P-a -v "$bin:/sisyphus:ro" "$IMAGE" \
	/sisyphus/sisyphusd run --role worker --coordinator coordinator:7700 --join "$north_invitation" --name stray >/dev/null
sleep 3
if on coordinator nodes | grep -q stray; then bad "a used invitation is refused"; else ok "a used invitation is refused"; fi
docker rm -f $P-stray >/dev/null 2>&1

echo "a client on another machine"
docker run -d --name $P-laptop --hostname laptop --network $P-b -v "$bin:/sisyphus:ro" "$IMAGE" sleep 86400 >/dev/null
check "a machine that has not joined is refused" bash -c "! docker exec $P-laptop /sisyphus/sisyphusd nodes --addr coordinator:7700"
client_invitation=$(on coordinator pool invite --role client | tail -1)
check "it joins as a client" on laptop pool join --addr coordinator:7700 "$client_invitation"
check "and then sees the pool's workers" bash -c "docker exec $P-laptop /sisyphus/sisyphusd nodes --addr coordinator:7700 | grep -q south"

echo "jobs from the client, run by both workers"
out=$(on laptop job submit --addr coordinator:7700 --workload primes --params '{"from":0,"to":20000000}' --tasks 8 2>&1) || true
if echo "$out" | grep -q '"count":1270607'; then ok "primes below 20,000,000 counted: 1,270,607"; else bad "primes job: $(echo "$out" | tail -2)"; fi
if echo "$out" | grep -q "on north" && echo "$out" | grep -q "on south"; then ok "its tasks ran on both workers"; else bad "its tasks ran on both workers"; fi

cid=$(on laptop blob put --addr coordinator:7700 /sisyphus/boulder.txt | tail -1)
out=$(on laptop job submit --addr coordinator:7700 --workload wordcount --params "{\"input\":\"$cid\"}" --tasks 4 2>&1) || true
if echo "$out" | grep -q '"words"'; then ok "a file stored by the client was read by the workers and its words counted"; else bad "wordcount job: $(echo "$out" | tail -2)"; fi

echo "a worker is lost while a job runs"
job=$(on laptop job submit --addr coordinator:7700 --workload primes --params '{"from":0,"to":3000000000}' --tasks 8 --detach | tail -1)
sleep 2
docker kill $P-south >/dev/null
finished() { on laptop job get --addr coordinator:7700 "$job" | grep -q succeeded; }
if until_ok 180 finished; then ok "the job finished all the same, its tasks done again on the worker left"; else bad "the job finished after losing a worker: $(on laptop job get --addr coordinator:7700 "$job" | head -3)"; fi

echo "the coordinator is stopped and started again"
docker restart $P-coordinator >/dev/null
back() { on coordinator nodes | grep -q north; }
if until_ok 60 back; then ok "the worker that was left came back to it unasked"; else bad "the worker came back after the coordinator restarted"; fi
check "and the job it had finished is still on record" bash -c "docker exec $P-laptop /sisyphus/sisyphusd job get --addr coordinator:7700 $job | grep -q succeeded"

if [ -n "$MODEL" ]; then
	echo "a worker's models, used from the client's machine"
	serves() { on coordinator nodes | grep north | grep -q chat; }
	if until_ok 20 serves; then ok "north offers the models of this machine's Ollama"; else bad "north offers models (is Ollama listening on more than 127.0.0.1?)"; fi
	docker exec -d $P-laptop /sisyphus/sisyphusd inference --addr coordinator:7700 --listen 127.0.0.1:11435
	sleep 3
	key=$(docker exec $P-laptop sh -c 'cat "$(/sisyphus/sisyphusd data-dir)/api.token"')
	listed=$(docker exec $P-laptop wget -qO- --header "Authorization: Bearer $key" http://127.0.0.1:11435/v1/models 2>&1) || true
	if echo "$listed" | grep -q "\"$MODEL\""; then ok "the client's own endpoint lists $MODEL"; else bad "the client's endpoint lists $MODEL: $(echo "$listed" | head -c 200)"; fi
	answer=$(docker exec $P-laptop wget -qO- -T 300 --header "Authorization: Bearer $key" --header "Content-Type: application/json" \
		--post-data "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with one word: what does Sisyphus push?\"}]}" \
		http://127.0.0.1:11435/v1/chat/completions 2>&1) || true
	if echo "$answer" | grep -q '"content"'; then ok "and $MODEL answered through the pool: $(echo "$answer" | sed 's/.*"content":"\([^"]*\)".*/\1/' | head -c 80)"; else bad "a reply through the pool: $(echo "$answer" | head -c 200)"; fi
fi

echo
echo "$passed passed, $failed failed"
[ -z "${KEEP:-}" ] || echo "the pool is left running; clear it up with: examples/rehearsal.sh down"
[ "$failed" -eq 0 ]
