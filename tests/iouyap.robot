*** Settings ***
Documentation       Deploys IOL nodes with the iouyap build under test bind-mounted and
...                 checks management access, untagged and 802.1Q forwarding, a relocated
...                 management interface, failure modes, and link changes via clab apply.
Library             OperatingSystem
Library             String

Suite Setup         Deploy Lab
Suite Teardown      Destroy Lab


*** Variables ***
${CLAB_BIN}         sudo -E containerlab
${LAB_FILE}         ${CURDIR}/iouyap.clab.yml
${APPLY_FILE}       ${CURDIR}/iouyap-apply.clab.yml
${IOUYAP_BIN}       ${CURDIR}/../iouyap
${R1}               clab-iouyap-r1
${R2}               clab-iouyap-r2
${R3}               clab-iouyap-r3
${SSH}              sshpass -p admin ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
...                 -o LogLevel=ERROR -o ConnectTimeout=10 -o PubkeyAuthentication=no
# Matches the iouyap process, not the shell running the match.
${FIND_IOUYAP}      for p in /proc/[0-9]*; do grep -qa '^/usr/bin/iouyap' $p/cmdline 2>/dev/null && echo $\{p#/proc/}; done


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
    Wait Until Keyword Succeeds    5 min    10s    Interface Has Mgmt IP    ${R2}    Ethernet0/0

Management reachable on relocated interface
    Wait Until Keyword Succeeds    5 min    10s    Interface Has Mgmt IP    ${R1}    Ethernet1/2

Untagged forwarding
    Ping Succeeds    ${R1}    10.1.0.2

802.1Q tagged forwarding
    Ping Succeeds    ${R1}    10.2.0.2

Forwarding on Ethernet0/0 freed by relocated management
    Ping Succeeds    ${R1}    10.0.0.2

Mismatched VLAN IDs do not forward
    [Documentation]    r1 tags with VLAN 200 and r2 with VLAN 300 on the same link.
    Ping Fails    ${R1}    10.4.0.2

Relocated management is unreachable without CLAB_IOL_MGMT_INTF
    [Documentation]    iouyap maps eth0 to Ethernet0/0 while IOL expects management on
    ...    Ethernet1/2. Remove this test once containerlab exports CLAB_IOL_MGMT_INTF.
    Wait Until Keyword Succeeds    5 min    10s    IOL Has Booted    ${R3}
    ${ip} =    Mgmt IP    ${R3}
    Host Ping Fails    ${ip}

Second iouyap instance is refused
    ${rc}    ${out} =    Run And Return Rc And Output    docker exec ${R2} /usr/bin/iouyap 513
    Should Not Be Equal As Integers    ${rc}    0
    Should Contain    ${out}    already has a lock

Management stops without iouyap and recovers on restart
    ${ip} =    Mgmt IP    ${R2}
    Wait Until Keyword Succeeds    1 min    5s    Host Ping Succeeds    ${ip}
    Run    docker exec ${R2} bash -c 'kill $(${FIND_IOUYAP})'
    ${pids} =    Run    docker exec ${R2} bash -c '${FIND_IOUYAP}'
    Should Be Empty    ${pids}
    Host Ping Fails    ${ip}
    ${rc} =    Run And Return Rc    docker exec -d -w /iol ${R2} /usr/bin/iouyap 513 -q
    Should Be Equal As Integers    ${rc}    0
    Wait Until Keyword Succeeds    1 min    5s    Host Ping Succeeds    ${ip}

Apply adds and removes links
    [Documentation]    IOL nodes are recreated by clab apply; r2 gains a second slot.
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    IOUYAP_BIN=${IOUYAP_BIN} ${CLAB_BIN} deploy -t ${APPLY_FILE}
    Log    ${out}
    Should Be Equal As Integers    ${rc}    0
    Wait Until Keyword Succeeds    5 min    10s    Interface Has Mgmt IP    ${R1}    Ethernet1/2
    Wait Until Keyword Succeeds    5 min    10s    Interface Has Mgmt IP    ${R2}    Ethernet0/0
    Configure    ${R1}    interface Ethernet0/3    ip address 10.3.0.1 255.255.255.0    no shutdown
    Configure    ${R2}    interface Ethernet1/0    ip address 10.3.0.2 255.255.255.0    no shutdown

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
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    IOUYAP_BIN=${IOUYAP_BIN} ${CLAB_BIN} deploy -t ${LAB_FILE} --reconfigure
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

IOL Has Booted
    [Arguments]    ${node}
    ${logs} =    Run    docker logs ${node} 2>&1
    Should Contain    ${logs}    %SYS-5-RESTART

Host Ping Succeeds
    [Arguments]    ${ip}
    ${rc} =    Run And Return Rc    ping -c 3 -W 1 ${ip}
    Should Be Equal As Integers    ${rc}    0

Host Ping Fails
    [Arguments]    ${ip}
    ${rc} =    Run And Return Rc    ping -c 5 -W 1 ${ip}
    Should Not Be Equal As Integers    ${rc}    0    ${ip} answered pings

IOS
    [Documentation]    Runs one exec command over SSH. Stdin stays open for ${hold}
    ...    seconds because IOS ends the session, and drops pending output, on stdin EOF.
    [Arguments]    ${node}    ${command}    ${hold}=5
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    sleep ${hold} | ${SSH} admin@${node} "${command}"
    Log    ${out}
    RETURN    ${out}

Configure
    [Documentation]    Enters each line in configuration mode over an interactive session.
    [Arguments]    ${node}    @{lines}
    ${cmds} =    Catenate    SEPARATOR=' '    conf t    @{lines}    end
    ${rc}    ${out} =    Run And Return Rc And Output
    ...    (printf '%s\\n' '${cmds}'; sleep 4; printf 'exit\\n') | ${SSH} -tt admin@${node}
    Log    ${out}

Interface Has Mgmt IP
    [Arguments]    ${node}    ${intf}
    ${out} =    IOS    ${node}    show ip interface brief
    Should Match Regexp    ${out}    ${intf}\\s+172\\.20\\.20\\.\\d+\\s+YES\\s+\\S+\\s+up\\s+up

Ping Succeeds
    [Documentation]    The first ping resolves ARP, so only the second must be loss-free.
    [Arguments]    ${node}    ${target}
    IOS    ${node}    ping ${target} repeat 5    hold=15
    ${out} =    IOS    ${node}    ping ${target} repeat 20    hold=20
    Should Contain    ${out}    Success rate is 100 percent

Ping Fails
    [Arguments]    ${node}    ${target}
    ${out} =    IOS    ${node}    ping ${target} repeat 5 timeout 1    hold=15
    Should Contain    ${out}    Success rate is 0 percent
