/*
 * Copyright 2026 Abhinav Gautam
 * SPDX-License-Identifier: Apache-2.0
 *
 * Reference IEC 60870-5 stations on lib60870-C (github.com/mz-automation/
 * lib60870, GPLv3) for the interop tests. Built and run only in a test
 * container; nothing of lib60870 is distributed with the Go modules.
 *
 *   peer server104 [tls] [mode=single|conn|multi] [files=1]
 *                                   controlled station on port 2404 (19998 with TLS), CA 1
 *   peer client104 <host> <port> [tls]
 *                                   controlling station, CA 1 (or as commanded)
 *   peer slave101 <tty> <link> [options]
 *                                   IEC 101 secondary (or balanced station B), CA 1
 *   peer master101 <tty> <link> [options]
 *                                   IEC 101 primary polling <link> (or balanced station A)
 *
 * IEC 101 options: bal=1 (balanced), la=<0|1|2> link address octets,
 * cot=<1|2>, ca=<1|2>, ioa=<1|2|3> (defaults: unbalanced, 1, 1, 1, 2).
 * Every event is printed on a line starting "PEER ". Commands come on
 * standard input, one per line:
 *
 *   spont <ioa> <float>             (server104, slave101) spontaneous M_ME_NC_1
 *   startdt | stopdt | gi | clock   (client104, master101)
 *   sc <ioa> <0|1> <select 0|1>     single command C_SC_NA_1
 *   dc <ioa> <1|2> <select 0|1>     double command C_DC_NA_1
 *   se <ioa> <float> <select 0|1>   set-point C_SE_NC_1
 *   send type=<n> [ioa= v= q= sel= qu= t=<CP56 as 14 hex digits> el=<ms>
 *        seq= cd= tr= cot= ca= oa= neg= test= sq= n=]
 *                                   any ASDU: queued by a controlled station,
 *                                   sent by a controlling station
 *   clockneg <0|1>                  (server104, slave101) refuse clock synchronisation
 *   quit
 *
 * Received information objects are printed with their elements read
 * through lib60870's getters (value, quality, select, qualifier, time tag
 * octets in hex). TLS files are /certs/ca.pem, /certs/peer.pem and
 * /certs/peer.key.
 */

#include <pthread.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "cs101_file_service.h"
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
static char inQueue[64][512];
static int inHead = 0, inTail = 0;
static volatile bool running = true;
static volatile bool clockNegative = false;

static void* readInput(void* arg)
{
    (void)arg;
    char line[512];
    while (fgets(line, sizeof(line), stdin)) {
        line[strcspn(line, "\r\n")] = 0;
        pthread_mutex_lock(&inLock);
        strncpy(inQueue[inTail % 64], line, 511);
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

/* ---- key=value arguments ---- */

/* kv finds " key=" (or "key=" at the start) in s and returns the value. */
static const char* kv(const char* s, const char* key)
{
    size_t n = strlen(key);
    for (const char* p = s; (p = strstr(p, key)) != NULL; p++) {
        if ((p == s || p[-1] == ' ') && p[n] == '=')
            return p + n + 1;
    }
    return NULL;
}

static long kvi(const char* s, const char* key, long def)
{
    const char* v = kv(s, key);
    return v ? strtol(v, NULL, 0) : def;
}

static double kvd(const char* s, const char* key, double def)
{
    const char* v = kv(s, key);
    return v ? strtod(v, NULL) : def;
}

/* argOpt reads key=value options from argv. */
static long argOpt(int argc, char** argv, const char* key, long def)
{
    size_t n = strlen(key);
    for (int i = 1; i < argc; i++)
        if (strncmp(argv[i], key, n) == 0 && argv[i][n] == '=')
            return strtol(argv[i] + n + 1, NULL, 0);
    return def;
}

static bool argFlag(int argc, char** argv, const char* flag)
{
    for (int i = 1; i < argc; i++)
        if (strcmp(argv[i], flag) == 0)
            return true;
    return false;
}

static void hex(char* out, const uint8_t* b, int n)
{
    for (int i = 0; i < n; i++)
        sprintf(out + 2 * i, "%02x", b[i]);
    out[2 * n] = 0;
}

/* fields writes a CP56Time2a as lib60870's getters read it:
 * ms-within-minute,minute,hour,day,day-of-week,month,year,IV,SU,RES1. */
static void fields(char* out, CP56Time2a t)
{
    sprintf(out, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%d", CP56Time2a_getSecond(t) * 1000 + CP56Time2a_getMillisecond(t),
            CP56Time2a_getMinute(t), CP56Time2a_getHour(t), CP56Time2a_getDayOfMonth(t), CP56Time2a_getDayOfWeek(t),
            CP56Time2a_getMonth(t), CP56Time2a_getYear(t), CP56Time2a_isInvalid(t) ? 1 : 0,
            CP56Time2a_isSummerTime(t) ? 1 : 0, CP56Time2a_isSubstituted(t) ? 1 : 0);
}

/* timeArg fills t from t=<14 hex digits>, or with the current time. */
static void timeArg(const char* line, struct sCP56Time2a* t)
{
    const char* v = kv(line, "t");
    memset(t, 0, sizeof(*t));
    if (v && strlen(v) >= 14) {
        for (int i = 0; i < 7; i++) {
            unsigned x = 0;
            sscanf(v + 2 * i, "%2x", &x);
            t->encodedValue[i] = (uint8_t)x;
        }
        return;
    }
    CP56Time2a_createFromMsTimestamp(t, Hal_getTimeInMs());
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

/* ---- Printing received information objects ---- */

static void printIO(int type, CS101_ASDU asdu, InformationObject io)
{
    double v = 0;
    int quality = 0, select = -1, qu = -1, seq = -1, el = -1, cd = -1, tr = -1;
    bool hasTime = false;
    char t56[16] = "-", t24[8] = "-";
    CP56Time2a ts = NULL;
    CP24Time2a ts24 = NULL;
    BinaryCounterReading bcr = NULL;
    switch (type) {
    case M_SP_NA_1: case M_SP_TA_1: case M_SP_TB_1:
        v = SinglePointInformation_getValue((SinglePointInformation)io);
        quality = SinglePointInformation_getQuality((SinglePointInformation)io);
        if (type == M_SP_TA_1) ts24 = SinglePointWithCP24Time2a_getTimestamp((SinglePointWithCP24Time2a)io);
        if (type == M_SP_TB_1) ts = SinglePointWithCP56Time2a_getTimestamp((SinglePointWithCP56Time2a)io);
        break;
    case M_DP_NA_1: case M_DP_TA_1: case M_DP_TB_1:
        v = DoublePointInformation_getValue((DoublePointInformation)io);
        quality = DoublePointInformation_getQuality((DoublePointInformation)io);
        if (type == M_DP_TA_1) ts24 = DoublePointWithCP24Time2a_getTimestamp((DoublePointWithCP24Time2a)io);
        if (type == M_DP_TB_1) ts = DoublePointWithCP56Time2a_getTimestamp((DoublePointWithCP56Time2a)io);
        break;
    case M_ST_NA_1: case M_ST_TA_1: case M_ST_TB_1:
        v = StepPositionInformation_getValue((StepPositionInformation)io);
        tr = StepPositionInformation_isTransient((StepPositionInformation)io);
        quality = StepPositionInformation_getQuality((StepPositionInformation)io);
        if (type == M_ST_TA_1) ts24 = StepPositionWithCP24Time2a_getTimestamp((StepPositionWithCP24Time2a)io);
        if (type == M_ST_TB_1) ts = StepPositionWithCP56Time2a_getTimestamp((StepPositionWithCP56Time2a)io);
        break;
    case M_BO_NA_1: case M_BO_TA_1: case M_BO_TB_1:
        v = BitString32_getValue((BitString32)io);
        quality = BitString32_getQuality((BitString32)io);
        if (type == M_BO_TA_1) ts24 = Bitstring32WithCP24Time2a_getTimestamp((Bitstring32WithCP24Time2a)io);
        if (type == M_BO_TB_1) ts = Bitstring32WithCP56Time2a_getTimestamp((Bitstring32WithCP56Time2a)io);
        break;
    case M_ME_NA_1: case M_ME_TA_1: case M_ME_TD_1:
        v = MeasuredValueNormalized_getValue((MeasuredValueNormalized)io);
        quality = MeasuredValueNormalized_getQuality((MeasuredValueNormalized)io);
        if (type == M_ME_TA_1) ts24 = MeasuredValueNormalizedWithCP24Time2a_getTimestamp((MeasuredValueNormalizedWithCP24Time2a)io);
        if (type == M_ME_TD_1) ts = MeasuredValueNormalizedWithCP56Time2a_getTimestamp((MeasuredValueNormalizedWithCP56Time2a)io);
        break;
    case M_ME_ND_1:
        v = MeasuredValueNormalizedWithoutQuality_getValue((MeasuredValueNormalizedWithoutQuality)io);
        break;
    case M_ME_NB_1: case M_ME_TB_1: case M_ME_TE_1:
        v = MeasuredValueScaled_getValue((MeasuredValueScaled)io);
        quality = MeasuredValueScaled_getQuality((MeasuredValueScaled)io);
        if (type == M_ME_TB_1) ts24 = MeasuredValueScaledWithCP24Time2a_getTimestamp((MeasuredValueScaledWithCP24Time2a)io);
        if (type == M_ME_TE_1) ts = MeasuredValueScaledWithCP56Time2a_getTimestamp((MeasuredValueScaledWithCP56Time2a)io);
        break;
    case M_ME_NC_1: case M_ME_TC_1: case M_ME_TF_1:
        v = MeasuredValueShort_getValue((MeasuredValueShort)io);
        quality = MeasuredValueShort_getQuality((MeasuredValueShort)io);
        if (type == M_ME_TC_1) ts24 = MeasuredValueShortWithCP24Time2a_getTimestamp((MeasuredValueShortWithCP24Time2a)io);
        if (type == M_ME_TF_1) ts = MeasuredValueShortWithCP56Time2a_getTimestamp((MeasuredValueShortWithCP56Time2a)io);
        break;
    case M_IT_NA_1: case M_IT_TA_1: case M_IT_TB_1:
        bcr = IntegratedTotals_getBCR((IntegratedTotals)io);
        v = BinaryCounterReading_getValue(bcr);
        seq = BinaryCounterReading_getSequenceNumber(bcr);
        quality = (BinaryCounterReading_hasCarry(bcr) ? 0x02 : 0) | (BinaryCounterReading_isAdjusted(bcr) ? 0x04 : 0) |
                  (BinaryCounterReading_isInvalid(bcr) ? 0x80 : 0);
        if (type == M_IT_TA_1) ts24 = IntegratedTotalsWithCP24Time2a_getTimestamp((IntegratedTotalsWithCP24Time2a)io);
        if (type == M_IT_TB_1) ts = IntegratedTotalsWithCP56Time2a_getTimestamp((IntegratedTotalsWithCP56Time2a)io);
        break;
    case M_EP_TA_1: {
        SingleEvent e = EventOfProtectionEquipment_getEvent((EventOfProtectionEquipment)io);
        v = SingleEvent_getEventState(e);
        quality = SingleEvent_getQDP(e);
        el = CP16Time2a_getEplapsedTimeInMs(EventOfProtectionEquipment_getElapsedTime((EventOfProtectionEquipment)io));
        ts24 = EventOfProtectionEquipment_getTimestamp((EventOfProtectionEquipment)io);
        break;
    }
    case M_EP_TD_1: {
        EventOfProtectionEquipmentWithCP56Time2a x = (EventOfProtectionEquipmentWithCP56Time2a)io;
        SingleEvent e = EventOfProtectionEquipmentWithCP56Time2a_getEvent(x);
        v = SingleEvent_getEventState(e);
        quality = SingleEvent_getQDP(e);
        el = CP16Time2a_getEplapsedTimeInMs(EventOfProtectionEquipmentWithCP56Time2a_getElapsedTime(x));
        ts = EventOfProtectionEquipmentWithCP56Time2a_getTimestamp(x);
        break;
    }
    case M_EP_TB_1: {
        PackedStartEventsOfProtectionEquipment x = (PackedStartEventsOfProtectionEquipment)io;
        v = PackedStartEventsOfProtectionEquipment_getEvent(x);
        quality = PackedStartEventsOfProtectionEquipment_getQuality(x);
        el = CP16Time2a_getEplapsedTimeInMs(PackedStartEventsOfProtectionEquipment_getElapsedTime(x));
        ts24 = PackedStartEventsOfProtectionEquipment_getTimestamp(x);
        break;
    }
    case M_EP_TE_1: {
        PackedStartEventsOfProtectionEquipmentWithCP56Time2a x = (PackedStartEventsOfProtectionEquipmentWithCP56Time2a)io;
        v = PackedStartEventsOfProtectionEquipmentWithCP56Time2a_getEvent(x);
        quality = PackedStartEventsOfProtectionEquipmentWithCP56Time2a_getQuality(x);
        el = CP16Time2a_getEplapsedTimeInMs(PackedStartEventsOfProtectionEquipmentWithCP56Time2a_getElapsedTime(x));
        ts = PackedStartEventsOfProtectionEquipmentWithCP56Time2a_getTimestamp(x);
        break;
    }
    case M_EP_TC_1: {
        PackedOutputCircuitInfo x = (PackedOutputCircuitInfo)io;
        v = PackedOutputCircuitInfo_getOCI(x);
        quality = PackedOutputCircuitInfo_getQuality(x);
        el = CP16Time2a_getEplapsedTimeInMs(PackedOutputCircuitInfo_getOperatingTime(x));
        ts24 = PackedOutputCircuitInfo_getTimestamp(x);
        break;
    }
    case M_EP_TF_1: {
        PackedOutputCircuitInfoWithCP56Time2a x = (PackedOutputCircuitInfoWithCP56Time2a)io;
        v = PackedOutputCircuitInfoWithCP56Time2a_getOCI(x);
        quality = PackedOutputCircuitInfoWithCP56Time2a_getQuality(x);
        el = CP16Time2a_getEplapsedTimeInMs(PackedOutputCircuitInfoWithCP56Time2a_getOperatingTime(x));
        ts = PackedOutputCircuitInfoWithCP56Time2a_getTimestamp(x);
        break;
    }
    case M_PS_NA_1: {
        StatusAndStatusChangeDetection scd = PackedSinglePointWithSCD_getSCD((PackedSinglePointWithSCD)io);
        v = StatusAndStatusChangeDetection_getSTn(scd);
        cd = StatusAndStatusChangeDetection_getCDn(scd);
        quality = PackedSinglePointWithSCD_getQuality((PackedSinglePointWithSCD)io);
        break;
    }
    case C_SC_NA_1: case C_SC_TA_1:
        v = SingleCommand_getState((SingleCommand)io);
        select = SingleCommand_isSelect((SingleCommand)io);
        qu = SingleCommand_getQU((SingleCommand)io);
        if (type == C_SC_TA_1) ts = SingleCommandWithCP56Time2a_getTimestamp((SingleCommandWithCP56Time2a)io);
        break;
    case C_DC_NA_1: case C_DC_TA_1:
        v = DoubleCommand_getState((DoubleCommand)io);
        select = DoubleCommand_isSelect((DoubleCommand)io);
        qu = DoubleCommand_getQU((DoubleCommand)io);
        if (type == C_DC_TA_1) ts = DoubleCommandWithCP56Time2a_getTimestamp((DoubleCommandWithCP56Time2a)io);
        break;
    case C_RC_NA_1: case C_RC_TA_1:
        v = StepCommand_getState((StepCommand)io);
        select = StepCommand_isSelect((StepCommand)io);
        qu = StepCommand_getQU((StepCommand)io);
        if (type == C_RC_TA_1) ts = StepCommandWithCP56Time2a_getTimestamp((StepCommandWithCP56Time2a)io);
        break;
    case C_SE_NA_1: case C_SE_TA_1:
        v = SetpointCommandNormalized_getValue((SetpointCommandNormalized)io);
        select = SetpointCommandNormalized_isSelect((SetpointCommandNormalized)io);
        qu = SetpointCommandNormalized_getQL((SetpointCommandNormalized)io);
        if (type == C_SE_TA_1) ts = SetpointCommandNormalizedWithCP56Time2a_getTimestamp((SetpointCommandNormalizedWithCP56Time2a)io);
        break;
    case C_SE_NB_1: case C_SE_TB_1:
        v = SetpointCommandScaled_getValue((SetpointCommandScaled)io);
        select = SetpointCommandScaled_isSelect((SetpointCommandScaled)io);
        qu = SetpointCommandScaled_getQL((SetpointCommandScaled)io);
        if (type == C_SE_TB_1) ts = SetpointCommandScaledWithCP56Time2a_getTimestamp((SetpointCommandScaledWithCP56Time2a)io);
        break;
    case C_SE_NC_1: case C_SE_TC_1:
        v = SetpointCommandShort_getValue((SetpointCommandShort)io);
        select = SetpointCommandShort_isSelect((SetpointCommandShort)io);
        qu = SetpointCommandShort_getQL((SetpointCommandShort)io);
        if (type == C_SE_TC_1) ts = SetpointCommandShortWithCP56Time2a_getTimestamp((SetpointCommandShortWithCP56Time2a)io);
        break;
    case C_BO_NA_1:
        v = Bitstring32Command_getValue((Bitstring32Command)io);
        break;
    case C_BO_TA_1:
        v = Bitstring32CommandWithCP56Time2a_getValue((Bitstring32CommandWithCP56Time2a)io);
        ts = Bitstring32CommandWithCP56Time2a_getTimestamp((Bitstring32CommandWithCP56Time2a)io);
        break;
    case M_EI_NA_1:
        v = EndOfInitialization_getCOI((EndOfInitialization)io);
        break;
    case C_IC_NA_1:
        v = InterrogationCommand_getQOI((InterrogationCommand)io);
        break;
    case C_CI_NA_1:
        v = CounterInterrogationCommand_getQCC((CounterInterrogationCommand)io);
        break;
    case C_CS_NA_1:
        ts = ClockSynchronizationCommand_getTime((ClockSynchronizationCommand)io);
        break;
    case C_TS_NA_1:
        v = TestCommand_isValid((TestCommand)io) ? 0x55aa : 0;
        break;
    case C_RP_NA_1:
        v = ResetProcessCommand_getQRP((ResetProcessCommand)io);
        break;
    case C_CD_NA_1:
        el = CP16Time2a_getEplapsedTimeInMs(DelayAcquisitionCommand_getDelay((DelayAcquisitionCommand)io));
        break;
    case C_TS_TA_1:
        v = TestCommandWithCP56Time2a_getCounter((TestCommandWithCP56Time2a)io);
        ts = TestCommandWithCP56Time2a_getTimestamp((TestCommandWithCP56Time2a)io);
        break;
    case P_ME_NA_1:
        v = ParameterNormalizedValue_getValue((ParameterNormalizedValue)io);
        qu = ParameterNormalizedValue_getQPM((ParameterNormalizedValue)io);
        break;
    case P_ME_NB_1:
        v = ParameterScaledValue_getValue((ParameterScaledValue)io);
        qu = ParameterScaledValue_getQPM((ParameterScaledValue)io);
        break;
    case P_ME_NC_1:
        v = ParameterFloatValue_getValue((ParameterFloatValue)io);
        qu = ParameterFloatValue_getQPM((ParameterFloatValue)io);
        break;
    case P_AC_NA_1:
        qu = ParameterActivation_getQuality((ParameterActivation)io);
        break;
    default:
        break;
    }
    char tf[64] = "-";
    if (ts) {
        hasTime = true;
        hex(t56, ts->encodedValue, 7);
        fields(tf, ts);
    }
    if (ts24) {
        hasTime = true;
        hex(t24, ts24->encodedValue, 3);
    }
    say("ASDU type=%d cot=%d neg=%d ca=%d ioa=%d value=%.10g quality=%d select=%d time=%d qu=%d tr=%d seq=%d el=%d cd=%d "
        "t56=%s tf=%s t24=%s oa=%d test=%d sq=%d n=%d end",
        type, CS101_ASDU_getCOT(asdu), CS101_ASDU_isNegative(asdu) ? 1 : 0, CS101_ASDU_getCA(asdu),
        InformationObject_getObjectAddress(io), v, quality, select, hasTime ? 1 : 0, qu, tr, seq, el, cd, t56, tf, t24,
        CS101_ASDU_getOA(asdu), CS101_ASDU_isTest(asdu) ? 1 : 0, CS101_ASDU_isSequence(asdu) ? 1 : 0,
        CS101_ASDU_getNumberOfElements(asdu));
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
        printIO(type, asdu, io);
        InformationObject_destroy(io);
    }
    if (n == 0)
        say("ASDU type=%d cot=%d neg=%d ca=%d ioa=-1", type, CS101_ASDU_getCOT(asdu),
            CS101_ASDU_isNegative(asdu) ? 1 : 0, CS101_ASDU_getCA(asdu));
}

/* ---- Building information objects from "send" lines ---- */

static InformationObject buildIO(int type, const char* l, int ioa)
{
    double v = kvd(l, "v", 0);
    int q = (int)kvi(l, "q", 0), sel = (int)kvi(l, "sel", 0), qu = (int)kvi(l, "qu", 0);
    bool tr = kvi(l, "tr", 0) != 0;
    struct sCP56Time2a t;
    timeArg(l, &t);
    struct sCP24Time2a t24;
    memcpy(t24.encodedValue, t.encodedValue, 3);
    struct sCP16Time2a el;
    CP16Time2a_setEplapsedTimeInMs(&el, (int)kvi(l, "el", 0));
    struct sBinaryCounterReading bcr;
    BinaryCounterReading_create(&bcr, (int32_t)v, (int)kvi(l, "seq", 0), (q & 0x02) != 0, (q & 0x04) != 0, (q & 0x80) != 0);
    tSingleEvent ev = 0;
    SingleEvent_setEventState(&ev, (EventState)((int)v & 3));
    SingleEvent_setQDP(&ev, (QualityDescriptorP)q);
    struct sStatusAndStatusChangeDetection scd;
    uint16_t st = (uint16_t)v, cd = (uint16_t)kvi(l, "cd", 0);
    scd.encodedValue[0] = st & 0xff;
    scd.encodedValue[1] = st >> 8;
    scd.encodedValue[2] = cd & 0xff;
    scd.encodedValue[3] = cd >> 8;
    switch (type) {
    case M_SP_NA_1: return (InformationObject)SinglePointInformation_create(NULL, ioa, v != 0, q);
    case M_SP_TA_1: return (InformationObject)SinglePointWithCP24Time2a_create(NULL, ioa, v != 0, q, &t24);
    case M_SP_TB_1: return (InformationObject)SinglePointWithCP56Time2a_create(NULL, ioa, v != 0, q, &t);
    case M_DP_NA_1: return (InformationObject)DoublePointInformation_create(NULL, ioa, (DoublePointValue)(int)v, q);
    case M_DP_TA_1: return (InformationObject)DoublePointWithCP24Time2a_create(NULL, ioa, (DoublePointValue)(int)v, q, &t24);
    case M_DP_TB_1: return (InformationObject)DoublePointWithCP56Time2a_create(NULL, ioa, (DoublePointValue)(int)v, q, &t);
    case M_ST_NA_1: return (InformationObject)StepPositionInformation_create(NULL, ioa, (int)v, tr, q);
    case M_ST_TA_1: return (InformationObject)StepPositionWithCP24Time2a_create(NULL, ioa, (int)v, tr, q, &t24);
    case M_ST_TB_1: return (InformationObject)StepPositionWithCP56Time2a_create(NULL, ioa, (int)v, tr, q, &t);
    case M_BO_NA_1: return (InformationObject)BitString32_createEx(NULL, ioa, (uint32_t)v, q);
    case M_BO_TA_1: return (InformationObject)Bitstring32WithCP24Time2a_createEx(NULL, ioa, (uint32_t)v, q, &t24);
    case M_BO_TB_1: return (InformationObject)Bitstring32WithCP56Time2a_createEx(NULL, ioa, (uint32_t)v, q, &t);
    case M_ME_NA_1: return (InformationObject)MeasuredValueNormalized_create(NULL, ioa, (float)v, q);
    case M_ME_TA_1: return (InformationObject)MeasuredValueNormalizedWithCP24Time2a_create(NULL, ioa, (float)v, q, &t24);
    case M_ME_TD_1: return (InformationObject)MeasuredValueNormalizedWithCP56Time2a_create(NULL, ioa, (float)v, q, &t);
    case M_ME_ND_1: return (InformationObject)MeasuredValueNormalizedWithoutQuality_create(NULL, ioa, (float)v);
    case M_ME_NB_1: return (InformationObject)MeasuredValueScaled_create(NULL, ioa, (int)v, q);
    case M_ME_TB_1: return (InformationObject)MeasuredValueScaledWithCP24Time2a_create(NULL, ioa, (int)v, q, &t24);
    case M_ME_TE_1: return (InformationObject)MeasuredValueScaledWithCP56Time2a_create(NULL, ioa, (int)v, q, &t);
    case M_ME_NC_1: return (InformationObject)MeasuredValueShort_create(NULL, ioa, (float)v, q);
    case M_ME_TC_1: return (InformationObject)MeasuredValueShortWithCP24Time2a_create(NULL, ioa, (float)v, q, &t24);
    case M_ME_TF_1: return (InformationObject)MeasuredValueShortWithCP56Time2a_create(NULL, ioa, (float)v, q, &t);
    case M_IT_NA_1: return (InformationObject)IntegratedTotals_create(NULL, ioa, &bcr);
    case M_IT_TA_1: return (InformationObject)IntegratedTotalsWithCP24Time2a_create(NULL, ioa, &bcr, &t24);
    case M_IT_TB_1: return (InformationObject)IntegratedTotalsWithCP56Time2a_create(NULL, ioa, &bcr, &t);
    case M_EP_TA_1: return (InformationObject)EventOfProtectionEquipment_create(NULL, ioa, &ev, &el, &t24);
    case M_EP_TD_1: return (InformationObject)EventOfProtectionEquipmentWithCP56Time2a_create(NULL, ioa, &ev, &el, &t);
    case M_EP_TB_1:
        return (InformationObject)PackedStartEventsOfProtectionEquipment_create(NULL, ioa, (StartEvent)(int)v, q, &el, &t24);
    case M_EP_TE_1:
        return (InformationObject)PackedStartEventsOfProtectionEquipmentWithCP56Time2a_create(NULL, ioa, (StartEvent)(int)v, q, &el, &t);
    case M_EP_TC_1: return (InformationObject)PackedOutputCircuitInfo_create(NULL, ioa, (OutputCircuitInfo)(int)v, q, &el, &t24);
    case M_EP_TF_1:
        return (InformationObject)PackedOutputCircuitInfoWithCP56Time2a_create(NULL, ioa, (OutputCircuitInfo)(int)v, q, &el, &t);
    case M_PS_NA_1: return (InformationObject)PackedSinglePointWithSCD_create(NULL, ioa, &scd, q);
    case C_SC_NA_1: return (InformationObject)SingleCommand_create(NULL, ioa, v != 0, sel, qu);
    case C_SC_TA_1: return (InformationObject)SingleCommandWithCP56Time2a_create(NULL, ioa, v != 0, sel, qu, &t);
    case C_DC_NA_1: return (InformationObject)DoubleCommand_create(NULL, ioa, (int)v, sel, qu);
    case C_DC_TA_1: return (InformationObject)DoubleCommandWithCP56Time2a_create(NULL, ioa, (int)v, sel, qu, &t);
    case C_RC_NA_1: return (InformationObject)StepCommand_create(NULL, ioa, (StepCommandValue)(int)v, sel, qu);
    case C_RC_TA_1: return (InformationObject)StepCommandWithCP56Time2a_create(NULL, ioa, (StepCommandValue)(int)v, sel, qu, &t);
    case C_SE_NA_1: return (InformationObject)SetpointCommandNormalized_create(NULL, ioa, (float)v, sel, qu);
    case C_SE_TA_1: return (InformationObject)SetpointCommandNormalizedWithCP56Time2a_create(NULL, ioa, (float)v, sel, qu, &t);
    case C_SE_NB_1: return (InformationObject)SetpointCommandScaled_create(NULL, ioa, (int)v, sel, qu);
    case C_SE_TB_1: return (InformationObject)SetpointCommandScaledWithCP56Time2a_create(NULL, ioa, (int)v, sel, qu, &t);
    case C_SE_NC_1: return (InformationObject)SetpointCommandShort_create(NULL, ioa, (float)v, sel, qu);
    case C_SE_TC_1: return (InformationObject)SetpointCommandShortWithCP56Time2a_create(NULL, ioa, (float)v, sel, qu, &t);
    case C_BO_NA_1: return (InformationObject)Bitstring32Command_create(NULL, ioa, (uint32_t)v);
    case C_BO_TA_1: return (InformationObject)Bitstring32CommandWithCP56Time2a_create(NULL, ioa, (uint32_t)v, &t);
    case M_EI_NA_1: return (InformationObject)EndOfInitialization_create(NULL, (uint8_t)v);
    case C_IC_NA_1: return (InformationObject)InterrogationCommand_create(NULL, ioa, (uint8_t)v);
    case C_CI_NA_1: return (InformationObject)CounterInterrogationCommand_create(NULL, ioa, (uint8_t)v);
    case C_RD_NA_1: return (InformationObject)ReadCommand_create(NULL, ioa);
    case C_CS_NA_1: return (InformationObject)ClockSynchronizationCommand_create(NULL, ioa, &t);
    case C_TS_NA_1: return (InformationObject)TestCommand_create(NULL);
    case C_RP_NA_1: return (InformationObject)ResetProcessCommand_create(NULL, ioa, (uint8_t)v);
    case C_CD_NA_1: return (InformationObject)DelayAcquisitionCommand_create(NULL, ioa, &el);
    case C_TS_TA_1: return (InformationObject)TestCommandWithCP56Time2a_create(NULL, (uint16_t)v, &t);
    case P_ME_NA_1: return (InformationObject)ParameterNormalizedValue_create(NULL, ioa, (float)v, qu);
    case P_ME_NB_1: return (InformationObject)ParameterScaledValue_create(NULL, ioa, (int)v, qu);
    case P_ME_NC_1: return (InformationObject)ParameterFloatValue_create(NULL, ioa, (float)v, qu);
    case P_AC_NA_1: return (InformationObject)ParameterActivation_create(NULL, ioa, qu);
    default: return NULL;
    }
}

/* buildASDU makes the ASDU of a "send" line (NULL if not one). */
static CS101_ASDU buildASDU(CS101_AppLayerParameters al, const char* l, int defaultCA)
{
    if (strncmp(l, "send ", 5) != 0)
        return NULL;
    int type = (int)kvi(l, "type", 0);
    int defCot = (type >= 45 && type < 120) ? CS101_COT_ACTIVATION : CS101_COT_SPONTANEOUS;
    if (type == C_RD_NA_1)
        defCot = CS101_COT_REQUEST;
    int n = (int)kvi(l, "n", 1);
    bool sq = kvi(l, "sq", 0) != 0;
    CS101_ASDU a = CS101_ASDU_create(al, sq, (CS101_CauseOfTransmission)kvi(l, "cot", defCot), (int)kvi(l, "oa", 0),
                                     (int)kvi(l, "ca", defaultCA), kvi(l, "test", 0) != 0, kvi(l, "neg", 0) != 0);
    int ioa = (int)kvi(l, "ioa", 0);
    for (int i = 0; i < n; i++) {
        /* lib60870 accepts only consecutive addresses in a sequence. */
        InformationObject io = buildIO(type, l, ioa + i);
        if (!io) {
            say("SEND-UNSUPPORTED type=%d", type);
            CS101_ASDU_destroy(a);
            return NULL;
        }
        if (!CS101_ASDU_addInformationObject(a, io))
            say("SEND-FULL type=%d at %d", type, i);
        InformationObject_destroy(io);
    }
    return a;
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

static CS101_AppLayerParameters stationAL = NULL;

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

static bool onInterrogation(void* p, IMasterConnection con, CS101_ASDU asdu, uint8_t qoi)
{
    (void)p;
    say("GI qoi=%d ca=%d", qoi, CS101_ASDU_getCA(asdu));
    sendGI(con, asdu, stationAL);
    return true;
}

static bool onCounterInterrogation(void* p, IMasterConnection con, CS101_ASDU asdu, uint8_t qcc)
{
    (void)p;
    say("CI qcc=%d ca=%d", qcc, CS101_ASDU_getCA(asdu));
    IMasterConnection_sendACT_CON(con, asdu, false);
    int rqt = qcc & 0x3f;
    CS101_ASDU a = CS101_ASDU_create(stationAL, false, (CS101_CauseOfTransmission)(37 + (rqt == 5 ? 0 : rqt)), 0,
                                     CS101_ASDU_getCA(asdu), false, false);
    struct sBinaryCounterReading bcr;
    BinaryCounterReading_create(&bcr, 123456, 5, false, true, false);
    InformationObject io = (InformationObject)IntegratedTotals_create(NULL, 400, &bcr);
    CS101_ASDU_addInformationObject(a, io);
    InformationObject_destroy(io);
    IMasterConnection_sendASDU(con, a);
    CS101_ASDU_destroy(a);
    IMasterConnection_sendACT_TERM(con, asdu);
    return true;
}

static bool onRead(void* p, IMasterConnection con, CS101_ASDU asdu, int ioa)
{
    (void)p;
    say("READ ioa=%d", ioa);
    if (ioa != 100) {
        CS101_ASDU_setCOT(asdu, CS101_COT_UNKNOWN_IOA);
        CS101_ASDU_setNegative(asdu, true);
        IMasterConnection_sendASDU(con, asdu);
        return true;
    }
    CS101_ASDU a = CS101_ASDU_create(stationAL, false, CS101_COT_REQUEST, 0, CS101_ASDU_getCA(asdu), false, false);
    InformationObject io = (InformationObject)SinglePointInformation_create(NULL, 100, true, IEC60870_QUALITY_GOOD);
    CS101_ASDU_addInformationObject(a, io);
    InformationObject_destroy(io);
    IMasterConnection_sendASDU(con, a);
    CS101_ASDU_destroy(a);
    return true;
}

static bool onResetProcess(void* p, IMasterConnection con, CS101_ASDU asdu, uint8_t qrp)
{
    (void)p;
    say("RESET qrp=%d", qrp);
    IMasterConnection_sendACT_CON(con, asdu, qrp != 1 && qrp != 2);
    return true;
}

static bool onDelayAcquisition(void* p, IMasterConnection con, CS101_ASDU asdu, CP16Time2a delay)
{
    (void)p;
    say("DELAY cot=%d ms=%d", CS101_ASDU_getCOT(asdu), CP16Time2a_getEplapsedTimeInMs(delay));
    if (CS101_ASDU_getCOT(asdu) == CS101_COT_ACTIVATION)
        IMasterConnection_sendACT_CON(con, asdu, false);
    return true;
}

static bool onClockSync(void* p, IMasterConnection con, CS101_ASDU asdu, CP56Time2a t)
{
    (void)p;
    (void)con;
    (void)asdu;
    char h[16], f[64];
    hex(h, t->encodedValue, 7);
    fields(f, t);
    say("CLOCK %llu t56=%s tf=%s", (unsigned long long)CP56Time2a_toMsTimestamp(t), h, f);
    return !clockNegative;
}

static bool isCommand(int type)
{
    return (type >= C_SC_NA_1 && type <= C_BO_NA_1) || (type >= C_SC_TA_1 && type <= C_BO_TA_1);
}

static bool commandSelect(int type, InformationObject io)
{
    switch (type) {
    case C_SC_NA_1: case C_SC_TA_1: return SingleCommand_isSelect((SingleCommand)io);
    case C_DC_NA_1: case C_DC_TA_1: return DoubleCommand_isSelect((DoubleCommand)io);
    case C_RC_NA_1: case C_RC_TA_1: return StepCommand_isSelect((StepCommand)io);
    case C_SE_NA_1: case C_SE_TA_1: return SetpointCommandNormalized_isSelect((SetpointCommandNormalized)io);
    case C_SE_NB_1: case C_SE_TB_1: return SetpointCommandScaled_isSelect((SetpointCommandScaled)io);
    case C_SE_NC_1: case C_SE_TC_1: return SetpointCommandShort_isSelect((SetpointCommandShort)io);
    default: return false;
    }
}

static bool onASDU(void* p, IMasterConnection con, CS101_ASDU asdu)
{
    (void)p;
    int type = CS101_ASDU_getTypeID(asdu);
    if (type >= P_ME_NA_1 && type <= P_AC_NA_1) {
        printASDU(asdu);
        IMasterConnection_sendACT_CON(con, asdu, false);
        return true;
    }
    if (!isCommand(type))
        return false;
    printASDU(asdu);
    InformationObject io = CS101_ASDU_getElement(asdu, 0);
    int ioa = io ? InformationObject_getObjectAddress(io) : 0;
    bool select = io ? commandSelect(type, io) : false;
    if (io)
        InformationObject_destroy(io);
    if (ioa == 4999) {
        CS101_ASDU_setCOT(asdu, CS101_COT_UNKNOWN_IOA);
        CS101_ASDU_setNegative(asdu, true);
        IMasterConnection_sendASDU(con, asdu);
    } else if (CS101_ASDU_getCOT(asdu) == CS101_COT_ACTIVATION) {
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

/* stationInput turns a line into an ASDU to queue (NULL: none) and handles
 * the station switches. */
static CS101_ASDU stationInput(const char* line)
{
    int x = 0;
    if (strcmp(line, "quit") == 0) {
        running = false;
        return NULL;
    }
    if (sscanf(line, "clockneg %d", &x) == 1) {
        clockNegative = x != 0;
        say("CLOCKNEG %d", x);
        return NULL;
    }
    CS101_ASDU a = spontaneous(line);
    if (!a)
        a = buildASDU(stationAL, line, 1);
    return a;
}

static void onConnection(void* p, IMasterConnection con, CS104_PeerConnectionEvent ev)
{
    (void)p;
    (void)con;
    say("CONNECTION %d", (int)ev);
}

/* ---- File service: one file offered (CA 1, IOA 1000, NOF 1) and files
 * received at IOA 2000 ---- */

static int fileSize = 1200;
static const int sectionSize = 500;

static uint8_t fileByte(int i) { return (uint8_t)(i * 7 + 3); }

static uint64_t fileDate(CS101_IFileProvider self)
{
    (void)self;
    return 1760000000000ULL;
}

static int fileGetSize(CS101_IFileProvider self)
{
    (void)self;
    return fileSize;
}

static int fileSectionSize(CS101_IFileProvider self, int n)
{
    (void)self;
    int off = n * sectionSize;
    if (off >= fileSize)
        return -1;
    return fileSize - off < sectionSize ? fileSize - off : sectionSize;
}

static bool fileSegment(CS101_IFileProvider self, int n, int offset, int size, uint8_t* data)
{
    (void)self;
    for (int i = 0; i < size; i++)
        data[i] = fileByte(n * sectionSize + offset + i);
    return true;
}

static void fileDone(CS101_IFileProvider self, bool ok)
{
    (void)self;
    say("FILE-SENT ok=%d", ok ? 1 : 0);
}

static struct sCS101_IFileProvider offered = {1, 1000, 1, NULL, fileDate, fileGetSize, fileSectionSize, fileSegment, fileDone};

static CS101_IFileProvider nextFile(void* p, CS101_IFileProvider after)
{
    (void)p;
    return after ? NULL : &offered;
}

static CS101_IFileProvider getFile(void* p, int ca, int ioa, uint16_t nof, int* err)
{
    (void)p;
    if (ca != 1) {
        *err = 1;
        return NULL;
    }
    if (ioa != 1000) {
        *err = 2;
        return NULL;
    }
    (void)nof;
    return &offered;
}

static uint8_t rxBuf[65536 * 4];
static int rxLen = 0;

static void rxSegment(CS101_IFileReceiver self, uint8_t nos, int offset, int size, uint8_t* data)
{
    (void)self;
    (void)nos;
    (void)offset;
    if (rxLen + size <= (int)sizeof(rxBuf)) {
        memcpy(rxBuf + rxLen, data, size);
        rxLen += size;
    }
}

static void rxFinished(CS101_IFileReceiver self, CS101_FileErrorCode result)
{
    (void)self;
    uint32_t h = 0;
    for (int i = 0; i < rxLen; i++)
        h = h * 31 + rxBuf[i];
    say("FILE-RECEIVED result=%d len=%d hash=%u", (int)result, rxLen, h);
}

static struct sCS101_IFileReceiver receiver = {NULL, rxFinished, rxSegment};

static CS101_IFileReceiver fileReady(void* p, int ca, int ioa, uint16_t nof, int length, int* err)
{
    (void)p;
    say("FILE-READY ca=%d ioa=%d nof=%d len=%d", ca, ioa, nof, length);
    if (ioa != 2000) {
        *err = 2;
        return NULL;
    }
    rxLen = 0;
    return &receiver;
}

static struct sCS101_FilesAvailable available = {nextFile, getFile, NULL};

static int server104(int argc, char** argv)
{
    bool tls = argFlag(argc, argv, "tls");
    CS104_Slave s = tls ? CS104_Slave_createSecure(100, 100, tlsConfig()) : CS104_Slave_create(100, 100);
    CS104_Slave_setLocalAddress(s, "0.0.0.0");
    CS104_Slave_setLocalPort(s, tls ? 19998 : 2404);
    CS104_Slave_setMaxOpenConnections(s, 8);
    if (argFlag(argc, argv, "mode=conn")) {
        CS104_Slave_setServerMode(s, CS104_MODE_CONNECTION_IS_REDUNDANCY_GROUP);
    } else if (argFlag(argc, argv, "mode=multi")) {
        CS104_Slave_setServerMode(s, CS104_MODE_MULTIPLE_REDUNDANCY_GROUPS);
        CS104_RedundancyGroup other = CS104_RedundancyGroup_create("other");
        CS104_RedundancyGroup_addAllowedClient(other, "10.9.9.9");
        CS104_Slave_addRedundancyGroup(s, other);
        CS104_Slave_addRedundancyGroup(s, CS104_RedundancyGroup_create("catch-all"));
    } else {
        CS104_Slave_setServerMode(s, CS104_MODE_SINGLE_REDUNDANCY_GROUP);
    }
    stationAL = CS104_Slave_getAppLayerParameters(s);
    CS104_Slave_setInterrogationHandler(s, onInterrogation, NULL);
    CS104_Slave_setCounterInterrogationHandler(s, onCounterInterrogation, NULL);
    CS104_Slave_setReadHandler(s, onRead, NULL);
    CS104_Slave_setResetProcessHandler(s, onResetProcess, NULL);
    CS104_Slave_setDelayAcquisitionHandler(s, onDelayAcquisition, NULL);
    CS104_Slave_setClockSyncHandler(s, onClockSync, NULL);
    CS104_Slave_setASDUHandler(s, onASDU, NULL);
    CS104_Slave_setConnectionEventHandler(s, onConnection, NULL);
    fileSize = (int)argOpt(argc, argv, "filesize", 1200);
    CS101_FileServer fs = CS101_FileServer_create(stationAL);
    CS101_FileServer_setFilesAvailableIfc(fs, &available);
    CS101_FileServer_setFileReadyHandler(fs, fileReady, NULL);
    CS104_Slave_addPlugin(s, CS101_FileServer_getSlavePlugin(fs));
    CS104_Slave_start(s);
    if (!CS104_Slave_isRunning(s)) {
        say("START-FAILED");
        return 1;
    }
    say("READY");
    char line[512];
    while (running) {
        while (nextInput(line)) {
            CS101_ASDU a = stationInput(line);
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

/* linkParams applies the IEC 101 options to the parameters. */
static void linkParams(int argc, char** argv, CS101_AppLayerParameters al, LinkLayerParameters ll)
{
    al->sizeOfCOT = (int)argOpt(argc, argv, "cot", 1);
    al->sizeOfCA = (int)argOpt(argc, argv, "ca", 1);
    al->sizeOfIOA = (int)argOpt(argc, argv, "ioa", 2);
    ll->addressLength = (int)argOpt(argc, argv, "la", 1);
}

static void onLinkState(void* p, int address, LinkLayerState state)
{
    (void)p;
    say("LINK %d %d", address, (int)state);
}

static int slave101(const char* tty, int link, int argc, char** argv)
{
    bool bal = argOpt(argc, argv, "bal", 0) != 0;
    SerialPort port = SerialPort_create(tty, 9600, 8, 'E', 1);
    CS101_Slave s = CS101_Slave_create(port, NULL, NULL, bal ? IEC60870_LINK_LAYER_BALANCED : IEC60870_LINK_LAYER_UNBALANCED);
    CS101_Slave_setLinkLayerAddress(s, link);
    CS101_Slave_setLinkLayerAddressOtherStation(s, link);
    stationAL = CS101_Slave_getAppLayerParameters(s);
    linkParams(argc, argv, stationAL, CS101_Slave_getLinkLayerParameters(s));
    CS101_Slave_setLinkLayerStateChanged(s, onLinkState, NULL);
    CS101_Slave_setInterrogationHandler(s, onInterrogation, NULL);
    CS101_Slave_setCounterInterrogationHandler(s, onCounterInterrogation, NULL);
    CS101_Slave_setReadHandler(s, onRead, NULL);
    CS101_Slave_setResetProcessHandler(s, onResetProcess, NULL);
    CS101_Slave_setDelayAcquisitionHandler(s, onDelayAcquisition, NULL);
    CS101_Slave_setClockSyncHandler(s, onClockSync, NULL);
    CS101_Slave_setASDUHandler(s, onASDU, NULL);
    fileSize = (int)argOpt(argc, argv, "filesize", 1200);
    CS101_FileServer fs = CS101_FileServer_create(stationAL);
    CS101_FileServer_setFilesAvailableIfc(fs, &available);
    CS101_FileServer_setFileReadyHandler(fs, fileReady, NULL);
    CS101_Slave_addPlugin(s, CS101_FileServer_getSlavePlugin(fs));
    if (!SerialPort_open(port)) {
        say("SERIAL-FAILED %s", tty);
        return 1;
    }
    say("READY");
    char line[512];
    while (running) {
        CS101_Slave_run(s);
        while (nextInput(line)) {
            CS101_ASDU a = stationInput(line);
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
    CS101_AppLayerParameters al = CS104_Connection_getAppLayerParameters(c);
    char line[512];
    int ca = 1;
    while (running) {
        while (nextInput(line)) {
            if (strcmp(line, "quit") == 0)
                running = false;
            else if (sscanf(line, "ca %d", &ca) == 1)
                say("CA %d", ca);
            else if (strcmp(line, "startdt") == 0)
                CS104_Connection_sendStartDT(c);
            else if (strcmp(line, "stopdt") == 0)
                CS104_Connection_sendStopDT(c);
            else if (strcmp(line, "gi") == 0)
                say("SENT gi %d", CS104_Connection_sendInterrogationCommand(c, CS101_COT_ACTIVATION, ca, IEC60870_QOI_STATION));
            else if (strcmp(line, "clock") == 0) {
                struct sCP56Time2a t;
                uint64_t now = Hal_getTimeInMs();
                CP56Time2a_createFromMsTimestamp(&t, now);
                say("SENT clock %d %llu", CS104_Connection_sendClockSyncCommand(c, ca, &t), (unsigned long long)now);
            } else if (strncmp(line, "send ", 5) == 0) {
                CS101_ASDU a = buildASDU(al, line, ca);
                if (a) {
                    say("SENT %s %d", line, CS104_Connection_sendASDU(c, a));
                    CS101_ASDU_destroy(a);
                }
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

static int master101(const char* tty, int link, int argc, char** argv)
{
    bool bal = argOpt(argc, argv, "bal", 0) != 0;
    SerialPort port = SerialPort_create(tty, 9600, 8, 'E', 1);
    CS101_Master m = CS101_Master_create(port, NULL, NULL, bal ? IEC60870_LINK_LAYER_BALANCED : IEC60870_LINK_LAYER_UNBALANCED);
    CS101_AppLayerParameters al = CS101_Master_getAppLayerParameters(m);
    linkParams(argc, argv, al, CS101_Master_getLinkLayerParameters(m));
    CS101_Master_setASDUReceivedHandler(m, onReceived, NULL);
    CS101_Master_setLinkLayerStateChanged(m, onLinkState, NULL);
    if (bal) {
        CS101_Master_setOwnAddress(m, link);
        CS101_Master_useSlaveAddress(m, link);
    } else {
        CS101_Master_addSlave(m, link);
    }
    if (!SerialPort_open(port)) {
        say("SERIAL-FAILED %s", tty);
        return 1;
    }
    say("READY");
    char line[512];
    int ca = 1;
    uint64_t lastPoll = 0;
    while (running) {
        if (!bal && Hal_getTimeInMs() - lastPoll > 100) {
            CS101_Master_pollSingleSlave(m, link);
            lastPoll = Hal_getTimeInMs();
        }
        CS101_Master_run(m);
        /* Balanced: commands are queued as user data (isChannelReady is
         * for unbalanced lines only). */
        while ((bal || CS101_Master_isChannelReady(m, link)) && nextInput(line)) {
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
            } else if (strncmp(line, "send ", 5) == 0) {
                CS101_ASDU a = buildASDU(al, line, ca);
                if (a) {
                    CS101_Master_sendASDU(m, a);
                    CS101_ASDU_destroy(a);
                    say("SENT %s", line);
                }
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
        return server104(argc, argv);
    if (argc >= 4 && strcmp(argv[1], "client104") == 0)
        return client104(argv[2], atoi(argv[3]), argFlag(argc, argv, "tls"));
    if (argc >= 4 && strcmp(argv[1], "slave101") == 0)
        return slave101(argv[2], atoi(argv[3]), argc, argv);
    if (argc >= 4 && strcmp(argv[1], "master101") == 0)
        return master101(argv[2], atoi(argv[3]), argc, argv);
    fprintf(stderr, "usage: peer server104 [tls] [mode=..] | client104 <host> <port> [tls] | slave101 <tty> <link> [opts] | "
                    "master101 <tty> <link> [opts]\n");
    return 2;
}
