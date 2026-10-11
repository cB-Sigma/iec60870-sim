/*
 * Copyright 2026 Abhinav Gautam
 * SPDX-License-Identifier: Apache-2.0
 *
 * Reference IEC 60870-5 stations on lib60870-C (github.com/mz-automation/
 * lib60870, GPLv3) for the interop tests. Built and run only in a test
 * container; nothing of lib60870 is distributed with the Go modules.
 *
 *   peer server104 [tls]            controlled station on port 2404 (19998 with TLS), CA 1
 *   peer client104 <host> <port> [tls]
 *                                   controlling station, CA 1 (or as commanded)
 *   peer slave101 <tty> <link>      IEC 101 unbalanced secondary, CA 1
 *   peer master101 <tty> <link>     IEC 101 unbalanced primary polling <link>
 *
 * The IEC 101 modes use one-octet link addresses, COT and CA and two-octet
 * object addresses. Every event is printed on a line starting "PEER ".
 * Commands come on standard input, one per line:
 *
 *   spont <ioa> <float>             (server104, slave101) spontaneous M_ME_NC_1
 *   startdt | gi | clock            (client104, master101)
 *   sc <ioa> <0|1> <select 0|1>     single command C_SC_NA_1
 *   dc <ioa> <1|2> <select 0|1>     double command C_DC_NA_1
 *   se <ioa> <float> <select 0|1>   set-point C_SE_NC_1
 *   quit
 *
 * TLS files are /certs/ca.pem, /certs/peer.pem and /certs/peer.key.
 */

#include <pthread.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "cs101_master.h"
#include "cs101_slave.h"
#include "cs104_connection.h"
#include "cs104_slave.h"
#include "hal_serial.h"
#include "hal_thread.h"
#include "hal_time.h"
#include "tls_config.h"

static pthread_mutex_t outLock = PTHREAD_MUTEX_INITIALIZER;

#define say(...)                                                                                                       \
    do {                                                                                                               \
        pthread_mutex_lock(&outLock);                                                                                  \
        printf("PEER " __VA_ARGS__);                                                                                   \
        printf("\n");                                                                                                  \
        fflush(stdout);                                                                                                \
        pthread_mutex_unlock(&outLock);                                                                                \
    } while (0)

/* Standard input, read by a thread into a small queue. */
static pthread_mutex_t inLock = PTHREAD_MUTEX_INITIALIZER;
static char inQueue[64][256];
static int inHead = 0, inTail = 0;
static volatile bool running = true;

static void* readInput(void* arg)
{
    (void)arg;
    char line[256];
    while (fgets(line, sizeof(line), stdin)) {
        line[strcspn(line, "\r\n")] = 0;
        pthread_mutex_lock(&inLock);
        strncpy(inQueue[inTail % 64], line, 255);
        inTail++;
        pthread_mutex_unlock(&inLock);
    }
    running = false;
    return NULL;
}

static bool nextInput(char* out)
{
    bool ok = false;
    pthread_mutex_lock(&inLock);
    if (inHead < inTail) {
        strcpy(out, inQueue[inHead % 64]);
        inHead++;
        ok = true;
    }
    pthread_mutex_unlock(&inLock);
    return ok;
}

static void onTLSEvent(void* p, TLSEventLevel level, int code, const char* msg, TLSConnection con)
{
    (void)p;
    (void)con;
    say("TLS-EVENT %d %d %s", (int)level, code, msg);
}

static TLSConfiguration tlsConfig(void)
{
    TLSConfiguration c = TLSConfiguration_create();
    TLSConfiguration_setEventHandler(c, onTLSEvent, NULL);
    /* lib60870 compares every key's bit length with one minimum (1024 by
     * default), which a 256-bit EC key fails. */
    TLSConfiguration_setMinimumKeyLength(c, 256);
    if (!TLSConfiguration_setOwnKeyFromFile(c, "/certs/peer.key", NULL) ||
        !TLSConfiguration_setOwnCertificateFromFile(c, "/certs/peer.pem") ||
        !TLSConfiguration_addCACertificateFromFile(c, "/certs/ca.pem")) {
        say("TLS-CONFIG-FAILED");
        exit(1);
    }
    TLSConfiguration_setChainValidation(c, true);
    TLSConfiguration_setAllowOnlyKnownCertificates(c, false);
    return c;
}

/* printASDU prints one line per information object. */
static void printASDU(CS101_ASDU asdu)
{
    int type = CS101_ASDU_getTypeID(asdu);
    int n = CS101_ASDU_getNumberOfElements(asdu);
    for (int i = 0; i < n; i++) {
        InformationObject io = CS101_ASDU_getElement(asdu, i);
        if (!io)
            continue;
        double v = 0;
        int quality = 0;
        int select = -1;
        bool hasTime = false;
        switch (type) {
        case M_SP_NA_1:
            v = SinglePointInformation_getValue((SinglePointInformation)io);
            quality = SinglePointInformation_getQuality((SinglePointInformation)io);
            break;
        case M_SP_TB_1:
            v = SinglePointInformation_getValue((SinglePointInformation)io);
            quality = SinglePointInformation_getQuality((SinglePointInformation)io);
            hasTime = true;
            break;
        case M_DP_NA_1:
            v = DoublePointInformation_getValue((DoublePointInformation)io);
            quality = DoublePointInformation_getQuality((DoublePointInformation)io);
            break;
        case M_DP_TB_1:
            v = DoublePointInformation_getValue((DoublePointInformation)io);
            quality = DoublePointInformation_getQuality((DoublePointInformation)io);
            hasTime = true;
            break;
        case M_ME_NC_1:
            v = MeasuredValueShort_getValue((MeasuredValueShort)io);
            quality = MeasuredValueShort_getQuality((MeasuredValueShort)io);
            break;
        case M_ME_TF_1:
            v = MeasuredValueShort_getValue((MeasuredValueShort)io);
            quality = MeasuredValueShort_getQuality((MeasuredValueShort)io);
            hasTime = true;
            break;
        case M_ME_NB_1:
            v = MeasuredValueScaled_getValue((MeasuredValueScaled)io);
            quality = MeasuredValueScaled_getQuality((MeasuredValueScaled)io);
            break;
        case M_IT_NA_1:
            v = BinaryCounterReading_getValue(IntegratedTotals_getBCR((IntegratedTotals)io));
            break;
        case C_SC_NA_1:
            v = SingleCommand_getState((SingleCommand)io);
            select = SingleCommand_isSelect((SingleCommand)io);
            break;
        case C_DC_NA_1:
            v = DoubleCommand_getState((DoubleCommand)io);
            select = DoubleCommand_isSelect((DoubleCommand)io);
            break;
        case C_SE_NC_1:
            v = SetpointCommandShort_getValue((SetpointCommandShort)io);
            select = SetpointCommandShort_isSelect((SetpointCommandShort)io);
            break;
        case C_IC_NA_1:
            v = InterrogationCommand_getQOI((InterrogationCommand)io);
            break;
        default:
            break;
        }
        say("ASDU type=%d cot=%d neg=%d ca=%d ioa=%d value=%g quality=%d select=%d time=%d", type,
            CS101_ASDU_getCOT(asdu), CS101_ASDU_isNegative(asdu) ? 1 : 0, CS101_ASDU_getCA(asdu),
            InformationObject_getObjectAddress(io), v, quality, select, hasTime ? 1 : 0);
        InformationObject_destroy(io);
    }
    if (n == 0)
        say("ASDU type=%d cot=%d neg=%d ca=%d ioa=-1", type, CS101_ASDU_getCOT(asdu),
            CS101_ASDU_isNegative(asdu) ? 1 : 0, CS101_ASDU_getCA(asdu));
}

/* command parses a command line into an information object (NULL if not one). */
static InformationObject command(const char* line)
{
    char what[16];
    int ioa = 0, sel = 0;
    double v = 0;
    if (sscanf(line, "%15s %d %lf %d", what, &ioa, &v, &sel) < 3)
        return NULL;
    if (strcmp(what, "sc") == 0)
        return (InformationObject)SingleCommand_create(NULL, ioa, v != 0, sel != 0, 0);
    if (strcmp(what, "dc") == 0)
        return (InformationObject)DoubleCommand_create(NULL, ioa, (int)v, sel != 0, 0);
    if (strcmp(what, "se") == 0)
        return (InformationObject)SetpointCommandShort_create(NULL, ioa, (float)v, sel != 0, 0);
    return NULL;
}

/* ---- Controlled station (IEC 104 server and IEC 101 slave) ---- */

static void sendGI(IMasterConnection con, CS101_ASDU req, CS101_AppLayerParameters al)
{
    IMasterConnection_sendACT_CON(con, req, false);
    CS101_ASDU a = CS101_ASDU_create(al, false, CS101_COT_INTERROGATED_BY_STATION, 0, CS101_ASDU_getCA(req), false, false);
    InformationObject io = (InformationObject)SinglePointInformation_create(NULL, 100, true, IEC60870_QUALITY_GOOD);
    CS101_ASDU_addInformationObject(a, io);
    InformationObject_destroy(io);
    IMasterConnection_sendASDU(con, a);
    CS101_ASDU_destroy(a);
    a = CS101_ASDU_create(al, false, CS101_COT_INTERROGATED_BY_STATION, 0, CS101_ASDU_getCA(req), false, false);
    io = (InformationObject)MeasuredValueShort_create(NULL, 200, 230.5f, IEC60870_QUALITY_GOOD);
    CS101_ASDU_addInformationObject(a, io);
    InformationObject_destroy(io);
    IMasterConnection_sendASDU(con, a);
    CS101_ASDU_destroy(a);
    IMasterConnection_sendACT_TERM(con, req);
}

static CS101_AppLayerParameters stationAL = NULL;

static bool onInterrogation(void* p, IMasterConnection con, CS101_ASDU asdu, uint8_t qoi)
{
    (void)p;
    say("GI qoi=%d ca=%d", qoi, CS101_ASDU_getCA(asdu));
    sendGI(con, asdu, stationAL);
    return true;
}

static bool onClockSync(void* p, IMasterConnection con, CS101_ASDU asdu, CP56Time2a t)
{
    (void)p;
    (void)con;
    (void)asdu;
    say("CLOCK %llu", (unsigned long long)CP56Time2a_toMsTimestamp(t));
    return true;
}

static bool onASDU(void* p, IMasterConnection con, CS101_ASDU asdu)
{
    (void)p;
    int type = CS101_ASDU_getTypeID(asdu);
    if (type != C_SC_NA_1 && type != C_DC_NA_1 && type != C_SE_NC_1)
        return false;
    printASDU(asdu);
    InformationObject io = CS101_ASDU_getElement(asdu, 0);
    bool select = false;
    if (type == C_SC_NA_1)
        select = SingleCommand_isSelect((SingleCommand)io);
    else if (type == C_DC_NA_1)
        select = DoubleCommand_isSelect((DoubleCommand)io);
    else
        select = SetpointCommandShort_isSelect((SetpointCommandShort)io);
    InformationObject_destroy(io);
    if (CS101_ASDU_getCOT(asdu) == CS101_COT_ACTIVATION) {
        IMasterConnection_sendACT_CON(con, asdu, false);
        if (!select)
            IMasterConnection_sendACT_TERM(con, asdu);
    } else if (CS101_ASDU_getCOT(asdu) == CS101_COT_DEACTIVATION) {
        CS101_ASDU_setCOT(asdu, CS101_COT_DEACTIVATION_CON);
        IMasterConnection_sendASDU(con, asdu);
    }
    return true;
}

static CS101_ASDU spontaneous(const char* line)
{
    int ioa = 0;
    double v = 0;
    if (sscanf(line, "spont %d %lf", &ioa, &v) != 2)
        return NULL;
    CS101_ASDU a = CS101_ASDU_create(stationAL, false, CS101_COT_SPONTANEOUS, 0, 1, false, false);
    InformationObject io = (InformationObject)MeasuredValueShort_create(NULL, ioa, (float)v, IEC60870_QUALITY_GOOD);
    CS101_ASDU_addInformationObject(a, io);
    InformationObject_destroy(io);
    return a;
}

static void onConnection(void* p, IMasterConnection con, CS104_PeerConnectionEvent ev)
{
    (void)p;
    (void)con;
    say("CONNECTION %d", (int)ev);
}

static int server104(bool tls)
{
    CS104_Slave s = tls ? CS104_Slave_createSecure(100, 100, tlsConfig()) : CS104_Slave_create(100, 100);
    CS104_Slave_setLocalAddress(s, "0.0.0.0");
    CS104_Slave_setLocalPort(s, tls ? 19998 : 2404);
    CS104_Slave_setServerMode(s, CS104_MODE_SINGLE_REDUNDANCY_GROUP);
    stationAL = CS104_Slave_getAppLayerParameters(s);
    CS104_Slave_setInterrogationHandler(s, onInterrogation, NULL);
    CS104_Slave_setClockSyncHandler(s, onClockSync, NULL);
    CS104_Slave_setASDUHandler(s, onASDU, NULL);
    CS104_Slave_setConnectionEventHandler(s, onConnection, NULL);
    CS104_Slave_start(s);
    if (!CS104_Slave_isRunning(s)) {
        say("START-FAILED");
        return 1;
    }
    say("READY");
    char line[256];
    while (running) {
        while (nextInput(line)) {
            if (strcmp(line, "quit") == 0)
                running = false;
            CS101_ASDU a = spontaneous(line);
            if (a) {
                CS104_Slave_enqueueASDU(s, a);
                CS101_ASDU_destroy(a);
                say("QUEUED %s", line);
            }
        }
        Thread_sleep(10);
    }
    CS104_Slave_stop(s);
    CS104_Slave_destroy(s);
    return 0;
}

static int slave101(const char* tty, int link)
{
    SerialPort port = SerialPort_create(tty, 9600, 8, 'E', 1);
    CS101_Slave s = CS101_Slave_create(port, NULL, NULL, IEC60870_LINK_LAYER_UNBALANCED);
    CS101_Slave_setLinkLayerAddress(s, link);
    CS101_Slave_setLinkLayerAddressOtherStation(s, link);
    stationAL = CS101_Slave_getAppLayerParameters(s);
    stationAL->sizeOfCOT = 1;
    stationAL->sizeOfCA = 1;
    stationAL->sizeOfIOA = 2;
    LinkLayerParameters ll = CS101_Slave_getLinkLayerParameters(s);
    ll->addressLength = 1;
    CS101_Slave_setInterrogationHandler(s, onInterrogation, NULL);
    CS101_Slave_setClockSyncHandler(s, onClockSync, NULL);
    CS101_Slave_setASDUHandler(s, onASDU, NULL);
    if (!SerialPort_open(port)) {
        say("SERIAL-FAILED %s", tty);
        return 1;
    }
    say("READY");
    char line[256];
    while (running) {
        CS101_Slave_run(s);
        while (nextInput(line)) {
            if (strcmp(line, "quit") == 0)
                running = false;
            CS101_ASDU a = spontaneous(line);
            if (a) {
                CS101_Slave_enqueueUserDataClass1(s, a);
                CS101_ASDU_destroy(a);
                say("QUEUED %s", line);
            }
        }
        Thread_sleep(1);
    }
    CS101_Slave_destroy(s);
    SerialPort_close(port);
    SerialPort_destroy(port);
    return 0;
}

/* ---- Controlling station (IEC 104 client and IEC 101 master) ---- */

static bool onReceived(void* p, int address, CS101_ASDU asdu)
{
    (void)p;
    (void)address;
    printASDU(asdu);
    return true;
}

static void onClientEvent(void* p, CS104_Connection con, CS104_ConnectionEvent ev)
{
    (void)p;
    (void)con;
    say("EVENT %d", (int)ev);
}

static int client104(const char* host, int port, bool tls)
{
    CS104_Connection c = tls ? CS104_Connection_createSecure(host, port, tlsConfig()) : CS104_Connection_create(host, port);
    CS104_Connection_setConnectionHandler(c, onClientEvent, NULL);
    CS104_Connection_setASDUReceivedHandler(c, onReceived, NULL);
    if (!CS104_Connection_connect(c)) {
        say("CONNECT-FAILED");
        return 1;
    }
    say("READY");
    char line[256];
    int ca = 1;
    while (running) {
        while (nextInput(line)) {
            if (strcmp(line, "quit") == 0)
                running = false;
            else if (sscanf(line, "ca %d", &ca) == 1)
                say("CA %d", ca);
            else if (strcmp(line, "startdt") == 0)
                CS104_Connection_sendStartDT(c);
            else if (strcmp(line, "gi") == 0)
                say("SENT gi %d", CS104_Connection_sendInterrogationCommand(c, CS101_COT_ACTIVATION, ca, IEC60870_QOI_STATION));
            else if (strcmp(line, "clock") == 0) {
                struct sCP56Time2a t;
                uint64_t now = Hal_getTimeInMs();
                CP56Time2a_createFromMsTimestamp(&t, now);
                say("SENT clock %d %llu", CS104_Connection_sendClockSyncCommand(c, ca, &t), (unsigned long long)now);
            } else {
                InformationObject io = command(line);
                if (io) {
                    say("SENT %s %d", line, CS104_Connection_sendProcessCommandEx(c, CS101_COT_ACTIVATION, ca, io));
                    InformationObject_destroy(io);
                }
            }
        }
        Thread_sleep(10);
    }
    CS104_Connection_destroy(c);
    return 0;
}

static void onLinkState(void* p, int address, LinkLayerState state)
{
    (void)p;
    say("LINK %d %d", address, (int)state);
}

static int master101(const char* tty, int link)
{
    SerialPort port = SerialPort_create(tty, 9600, 8, 'E', 1);
    CS101_Master m = CS101_Master_create(port, NULL, NULL, IEC60870_LINK_LAYER_UNBALANCED);
    CS101_AppLayerParameters al = CS101_Master_getAppLayerParameters(m);
    al->sizeOfCOT = 1;
    al->sizeOfCA = 1;
    al->sizeOfIOA = 2;
    LinkLayerParameters ll = CS101_Master_getLinkLayerParameters(m);
    ll->addressLength = 1;
    CS101_Master_setASDUReceivedHandler(m, onReceived, NULL);
    CS101_Master_setLinkLayerStateChanged(m, onLinkState, NULL);
    CS101_Master_addSlave(m, link);
    if (!SerialPort_open(port)) {
        say("SERIAL-FAILED %s", tty);
        return 1;
    }
    say("READY");
    char line[256];
    int ca = 1;
    uint64_t lastPoll = 0;
    while (running) {
        if (Hal_getTimeInMs() - lastPoll > 100) {
            CS101_Master_pollSingleSlave(m, link);
            lastPoll = Hal_getTimeInMs();
        }
        CS101_Master_run(m);
        while (CS101_Master_isChannelReady(m, link) && nextInput(line)) {
            CS101_Master_useSlaveAddress(m, link);
            if (strcmp(line, "quit") == 0)
                running = false;
            else if (sscanf(line, "ca %d", &ca) == 1)
                say("CA %d", ca);
            else if (strcmp(line, "gi") == 0) {
                CS101_Master_sendInterrogationCommand(m, CS101_COT_ACTIVATION, ca, IEC60870_QOI_STATION);
                say("SENT gi");
            } else if (strcmp(line, "clock") == 0) {
                struct sCP56Time2a t;
                uint64_t now = Hal_getTimeInMs();
                CP56Time2a_createFromMsTimestamp(&t, now);
                CS101_Master_sendClockSyncCommand(m, ca, &t);
                say("SENT clock %llu", (unsigned long long)now);
            } else {
                InformationObject io = command(line);
                if (io) {
                    CS101_Master_sendProcessCommand(m, CS101_COT_ACTIVATION, ca, io);
                    say("SENT %s", line);
                    InformationObject_destroy(io);
                }
            }
            CS101_Master_run(m);
        }
        Thread_sleep(1);
    }
    CS101_Master_destroy(m);
    SerialPort_close(port);
    SerialPort_destroy(port);
    return 0;
}

int main(int argc, char** argv)
{
    pthread_t in;
    pthread_create(&in, NULL, readInput, NULL);
    if (argc >= 2 && strcmp(argv[1], "server104") == 0)
        return server104(argc >= 3 && strcmp(argv[2], "tls") == 0);
    if (argc >= 4 && strcmp(argv[1], "client104") == 0)
        return client104(argv[2], atoi(argv[3]), argc >= 5 && strcmp(argv[4], "tls") == 0);
    if (argc >= 4 && strcmp(argv[1], "slave101") == 0)
        return slave101(argv[2], atoi(argv[3]));
    if (argc >= 4 && strcmp(argv[1], "master101") == 0)
        return master101(argv[2], atoi(argv[3]));
    fprintf(stderr, "usage: peer server104 [tls] | client104 <host> <port> [tls] | slave101 <tty> <link> | master101 <tty> <link>\n");
    return 2;
}
