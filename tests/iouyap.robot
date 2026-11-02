*** Settings ***
Documentation       Deploys IOL nodes with the iouyap build under test bind-mounted and
...                 checks management access, untagged and 802.1Q forwarding, a relocated
...                 management interface, IOL supervision, failure modes, and link changes
...                 via clab apply.
Library             OperatingSystem
Library             String

Suite Setup         Deploy Lab
Suite Teardown      Destroy Lab
Test Teardown       Run Keyword If Test Failed    Log iouyap State


*** Variables ***
${CLAB_BIN}         sudo -E containerlab
${LAB_FILE}         ${CURDIR}/iouyap.clab.yml
${APPLY_FILE}       ${CURDIR}/iouyap-apply.clab.yml
${IOUYAP_BIN}       ${CURDIR}/../iouyap
${BOOT_TIMEOUT}     5 min
${R1}               clab-iouyap-r1
${R2}               clab-iouyap-r2
${R3}               clab-iouyap-r3
${SSH}              sshpass -p admin ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
...                 -o LogLevel=ERROR -o ConnectTimeout=10 -o PubkeyAuthentication=no


*** Test Cases ***
Image does not ship iouyap
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    docker run --rm --entrypoint sh %{IOL_IMAGE=ghcr.io/clab-labs/cisco_iol:17.12.01-noiouyap} -c 'test -e /usr/bin/iouyap'
    Should Not Be Equal As Integers    ${rc}    0    image already contains /usr/bin/iouyap

Nodes run the iouyap build under test
    ${want} =    Sha256    ${IOUYAP_BIN}
    FOR    ${node}    IN    ${R1}    ${R2}    ${R3}
        ${rc}    ${got} =    Run And Return Rc And Output
        ...    docker exec ${node} sha256sum /usr/bin/iouyap | cut -d' ' -f1
        Should Be Equal As Integers    ${rc}    0
        Should Be Equal    ${got}    ${want}
    END

Management reachable on default interface
    Wait Until Keyword Succeeds    ${BOOT_TIMEOUT}    10s    Interface Has Mgmt IP    ${R2}    Ethernet0/0

Management reachable on relocated interface
    Wait Until Keyword Succeeds    ${BOOT_TIMEOUT}    10s    Interface Has Mgmt IP    ${R1}    Ethernet1/2

Untagged forwarding
    Ping Succeeds    ${R1}    10.1.0.2

802.1Q tagged forwarding
    Ping Succeeds    ${R1}    10.2.0.2

Forwarding on Ethernet0/0 freed by relocated management
    Ping Succeeds    ${R1}    10.0.0.2

Mismatched VLAN IDs do not forward
    [Documentation]    r1 tags with VLAN 200 and r2 with VLAN 300 on the same link.
    Ping Fails    ${R1}    10.4.0.2

containerlab exports the management interface
    [Documentation]    iouyap learns the relocated management port from CLAB_IOL_MGMT_INTF,
    ...    which containerlab sets from mgmt-intf.
    ${env} =    Run    docker exec ${R3} printenv CLAB_IOL_MGMT_INTF
    Should Be Equal    ${env}    Ethernet1/2
    Wait Until Keyword Succeeds    ${BOOT_TIMEOUT}    10s    Interface Has Mgmt IP    ${R3}    Ethernet1/2

Second iouyap instance is refused
    ${rc}    ${out} =    Run And Return Rc And Output    docker exec ${R2} /usr/bin/iouyap 513
    Should Not Be Equal As Integers    ${rc}    0
    Should Contain    ${out}    already has a lock

iouyap supervises IOL with every port in NETMAP
    [Documentation]    iouyap runs as PID 1 and starts IOL as its only child, with -e set
    ...    from CLAB_IOL_NUM_SLOTS (2 on r2) and NETMAP listing all 8 ports.
    ${netmap} =    Run    docker exec ${R2} cat /iol/NETMAP
    ${lines} =    Get Line Count    ${netmap}
    Should Be Equal As Integers    ${lines}    8
    Should Contain    ${netmap}    2:1/3 513:1/3
    ${init} =    Run    docker exec ${R2} bash -c 'xargs -0 < /proc/1/cmdline'
    Should Start With    ${init}    /usr/bin/iouyap
    ${iol} =    Run    docker exec ${R2} bash -c 'c=$(cat /proc/1/task/*/children); xargs -0 < /proc/$\{c%% *}/cmdline'
    Should Start With    ${iol}    /iol/iol.bin
    Should End With    ${iol}    -e 2

Console over telnet on the management address
    [Documentation]    Ctrl-C on the console reaches IOS as a key and must not stop IOL.
    ...    Enter goes first: after boot, IOS ignores console input until a lone Enter.
    ${ip} =    Mgmt IP    ${R2}
    Wait Until Keyword Succeeds    1 min    5s    Console Answers    ${ip}    \\r
    Console Answers    ${ip}    \\003\\r

Management address answers once
    [Documentation]    eth0 shares IOL's address and MAC, and only IOL may answer pings.
    ${ip} =    Mgmt IP    ${R2}
    ${mac} =    Run    docker exec ${R2} cat /sys/class/net/eth0/address
    Should Be Equal    ${mac}    aa:bb:cc:00:02:00
    ${rc}    ${out} =    Run And Return Rc And Output    ping -c 5 -i 0.3 ${ip}
    Should Be Equal As Integers    ${rc}    0
    Should Not Contain    ${out}    DUP!

Container stops when iouyap dies
    [Documentation]    Killed from the host: PID 1 ignores SIGKILL sent from inside
    ...    its own PID namespace.
    ${pid} =    Run    docker inspect -f '{{.State.Pid}}' ${R3}
    Run    sudo kill -9 ${pid}
    Wait Until Keyword Succeeds    30s    1s    Container Is Stopped    ${R3}

Apply adds and removes links
    [Documentation]    IOL nodes are recreated by clab apply. The configuration is saved
    ...    to NVRAM first: on recreate, containerlab rewrites the IOL boot config only
    ...    after the container has started, so IOL may boot without it.
    FOR    ${node}    IN    ${R1}    ${R2}
        ${out} =    IOS    ${node}    write memory    hold=10
        Should Contain    ${out}    [OK]
    END
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    ${CLAB_BIN} deploy -t ${APPLY_FILE}
    Log    ${out}
    Should Be Equal As Integers    ${rc}    0
    Wait Until Keyword Succeeds    ${BOOT_TIMEOUT}    10s    Interface Has Mgmt IP    ${R1}    Ethernet1/2
    Wait Until Keyword Succeeds    ${BOOT_TIMEOUT}    10s    Interface Has Mgmt IP    ${R2}    Ethernet0/0

Added link forwards after apply
    Ping Succeeds    ${R1}    10.3.0.2

Removed link does not forward after apply
    Ping Fails    ${R1}    10.1.0.2

Existing links forward after apply
    Ping Succeeds    ${R1}    10.0.0.2
    Ping Succeeds    ${R1}    10.2.0.2


*** Keywords ***
Deploy Lab
    File Should Exist    ${IOUYAP_BIN}
    # Every clab call re-reads the topology, which binds ${IOUYAP_BIN}.
    Set Environment Variable    IOUYAP_BIN    ${IOUYAP_BIN}
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    ${CLAB_BIN} deploy -t ${LAB_FILE} --reconfigure
    Log    ${out}
    Should Be Equal As Integers    ${rc}    0

Destroy Lab
    ${out} =    Run    ${CLAB_BIN} destroy -t ${LAB_FILE} --cleanup
    Log    ${out}

Sha256
    [Arguments]    ${path}
    ${rc}    ${sum} =    Run And Return Rc And Output    sha256sum ${path} | cut -d' ' -f1
    Should Be Equal As Integers    ${rc}    0
    RETURN    ${sum}

Mgmt IP
    [Arguments]    ${node}
    ${ip} =    Run    docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' ${node}
    Should Match Regexp    ${ip}    ^\\d+\\.\\d+\\.\\d+\\.\\d+$
    RETURN    ${ip}




IOS
    [Documentation]    Runs one exec command over SSH. Stdin stays open for ${hold}
    ...    seconds because IOS ends the session, and drops pending output, on stdin EOF.
    [Arguments]    ${node}    ${command}    ${hold}=5
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    sleep ${hold} | ${SSH} admin@${node} "${command}"
    Log    ${out}
    RETURN    ${out}

Interface Has Mgmt IP
    [Arguments]    ${node}    ${intf}
    ${out} =    IOS    ${node}    show ip interface brief
    Should Match Regexp    ${out}    ${intf}\\s+172\\.20\\.20\\.\\d+\\s+YES\\s+\\S+\\s+up\\s+up

Ping Succeeds
    [Documentation]    Warm-up pings repeat until one is answered, which also resolves
    ...    ARP; then 20 pings must all be answered.
    [Arguments]    ${node}    ${target}
    Wait Until Keyword Succeeds    1 min    1s    Ping Answers    ${node}    ${target}
    ${out} =    IOS    ${node}    ping ${target} repeat 20    hold=20
    Should Contain    ${out}    Success rate is 100 percent

Ping Answers
    [Arguments]    ${node}    ${target}
    ${out} =    IOS    ${node}    ping ${target} repeat 5    hold=15
    Should Match Regexp    ${out}    Success rate is [1-9]

Ping Fails
    [Arguments]    ${node}    ${target}
    ${out} =    IOS    ${node}    ping ${target} repeat 5 timeout 1    hold=25
    Should Contain    ${out}    Success rate is 0 percent

Log iouyap State
    [Documentation]    Logs per-port counters and the iouyap log of r1 and r2.
    FOR    ${node}    IN    ${R1}    ${R2}
        Run    docker kill -s USR1 ${node}
        Sleep    1s
        ${logs} =    Run    docker logs ${node} 2>&1 | grep 'level=' | tail -n 60
        Log    ${node}:\n${logs}    WARN
    END

Container Is Stopped
    [Arguments]    ${node}
    ${running} =    Run    docker inspect -f '{{.State.Running}}' ${node}
    Should Be Equal    ${running}    false

Console Answers
    [Documentation]    Sends ${keys} and then "show clock" over the telnet console at ${ip}.
    [Arguments]    ${ip}    ${keys}
    ${out} =    Run
    ...    (sleep 2; printf "${keys}"; sleep 2; printf "show clock\\r"; sleep 3) | telnet ${ip} 5000
    Log    ${out}
    Should Match Regexp    ${out}    [>#]show clock\\s+\\*?\\d+:\\d+:\\d+
