#!/bin/bash

# Get all the variables.
PROCESSES=$1

# Remove a previous postgres instance if it exists.
containers=$(docker ps --filter name=lnd-postgres -aq) 
if [ -n "$containers" ]; then
    echo "Removing previous postgres instance..."
    docker rm $containers --force --volumes 
fi

echo "Starting new postgres container"

for ((i=0; i<PROCESSES; i++)); do 
    port=$((6432 + i))
    container_name="lnd-postgres-$i"

    echo "Starting postgres container $container_name on port $port"

    # Start a fresh postgres instance. Allow a maximum of 500 connections so
    # that multiple lnd instances with a maximum number of connections of 20
    # each can run concurrently. Note that many of the settings here are
    # specifically for integration testing and are not fit for running
    # production nodes. The increase in max connections ensures that there are
    # enough entries allocated for the RWConflictPool to allow multiple
    # conflicting transactions to track serialization conflicts. The increase
    # in predicate locks and locks per transaction is to allow the queries to
    # lock individual rows instead of entire tables, helping reduce
    # serialization conflicts. Disabling sequential scan for small tables also
    # helps prevent serialization conflicts by ensuring lookups lock only
    # relevant rows in the index rather than the entire table.  
    docker run \
	    --name $container_name \
	    -p $port:5432 \
	    -e POSTGRES_PASSWORD=postgres \
	    -d postgres:13-alpine \
	    -N 1500 \
	    -c max_pred_locks_per_transaction=1024 \
	    -c max_locks_per_transaction=128 \
	    -c enable_seqscan=off 

    # docker logs -f lnd-postgres >itest/postgres.log 2>&1 &
done
