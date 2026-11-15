#!/bin/sh
# Runs the image IMAGE with soft as PID 1 (no tini, no --init), pushes PUSHES
# commits over SSH, and fails if any zombie process is left in the container
# or if `docker stop` does not end the container with status 0.
#
# Each push that updates a ref makes git (2.47 or newer) start a detached
# "git maintenance run --auto"; its orphan is re-parented to PID 1, and stays a
# zombie unless PID 1 reaps it.
#
# Usage: check-pid1-reaping.sh IMAGE [PUSHES]
set -eu

image=$1
pushes=${2:-10}
name=soft-pid1-check-$$
work=$(mktemp -d)
ssh_port=${SSH_PORT:-23299}
http_port=${HTTP_PORT:-23298}

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

ssh-keygen -q -t ed25519 -N '' -f "$work/key"
docker run -d --name "$name" --pids-limit 128 \
	-p "127.0.0.1:$ssh_port:23231" -p "127.0.0.1:$http_port:23232" \
	-e SOFT_SERVE_INITIAL_ADMIN_KEYS="$(cat "$work/key.pub")" \
	--entrypoint /usr/local/bin/soft "$image" serve >/dev/null

i=0
until curl -fsS "http://127.0.0.1:$http_port/readyz" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 60 ]; then
		docker logs "$name"
		echo "server did not become ready" >&2
		exit 1
	fi
	sleep 1
done

pid1=$(docker exec "$name" cat /proc/1/comm)
if [ "$pid1" != soft ]; then
	echo "PID 1 is $pid1, not soft" >&2
	exit 1
fi

export GIT_SSH_COMMAND="ssh -i $work/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
git init -q -b main "$work/repo"
cd "$work/repo"
git config user.name check
git config user.email check@example.com
git remote add origin "ssh://127.0.0.1:$ssh_port/pid1-check"
n=1
while [ "$n" -le "$pushes" ]; do
	echo "$n" >file
	git add file
	git commit -qm "commit $n"
	if ! git push -q origin main; then
		docker logs "$name" 2>&1 | tail -50
		exit 1
	fi
	n=$((n + 1))
done

# Give the detached maintenance processes time to exit.
sleep 3
zombies=$(docker exec "$name" ps -o stat,comm | awk 'NR > 1 && $1 ~ /^Z/' | wc -l)
echo "pushes: $pushes, zombies: $zombies"
if [ "$zombies" -ne 0 ]; then
	docker exec "$name" ps -o pid,ppid,stat,comm
	exit 1
fi

docker stop -t 10 "$name" >/dev/null
code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
echo "exit status after docker stop: $code"
if [ "$code" -ne 0 ]; then
	docker logs "$name" | tail -20
	exit 1
fi
