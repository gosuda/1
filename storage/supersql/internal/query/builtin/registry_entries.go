package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

type primitiveOp uint8

const (
	opMissing primitiveOp = iota
	opOutput
	opInput
	opLt
	opGt
	opEq
	opNe
	opMul
	opLe
	opGe
	opDiv
	opMod
	opPl
	opMi
	opUm
	opAbs
	opRound
	opTrunc
	opCast
	opOctets
	opSubstr
	opLength
	opCat
	opUp
	opLeft
	opRight
	opCmp
	opSign
	opTypmod
	opDivTrunc
	opSqrt
	opFloatCore
	opFloatTrig
	opCeil
	opFloor
	opBitAnd
	opBitOr
	opBitXor
	opBitNot
	opShiftLeft
	opShiftRight
	opGCD
	opLCM
	opLike
	opLikeEscape
	opTextSearch
	opTextPadding
	opTextCase
	opTextCode
	opDigest
	opBinaryEncoding
	opOverlay
	opByteAccess
	opArrayMetadata
	opArrayIO
	opBtreeComparison
	opArrayComparison
	opStringToArray
	opArrayToString
	opArrayTrim
	opArrayConstruction
	opArrayReverse
	opArraySearch
	opArrayReplace
	opArrayMembership
	opArrayFill
	opWidthBucketRange
	opWidthBucketArray
	opUUID
	opTextQuote
	opTypedQuote
	opNameIO
	opVariadicText
	opFormatText
	opRecordIO
	opRecordComparison
	opCString
	opNullCount
	opIntervalInput
	opIntervalOutput
	opIntervalArithmetic
	opIntervalTypmod
	opIntervalField
	opIntervalExtremum
	opIntervalSend
	opParseIdentifier
	opUnicodeString
	opUnicodeNormalization
	opUnicodeInfo
	opJSONText
	opJSONB
	opJSONTextExtract
	opJSONPath
	opJSONPathRuntime
	opJSONBExtract
	opJSONBExists
	opJSONBPath
	opJSONStrip
	opJSONBStrip
	opJSONBPretty
	opJSONBConcat
	opJSONBDelete
	opDate
	opDateOutput
	opDateFields
	opTime
	opTimestamp
	opTimestampOutput
	opTimeFields
	opTimestampFields
	opTimestampCalendar
	opTimestampBin
	opTemporalOverlaps
	opTimezone
	opOIDName
	opBinaryValue
	opNumericExtra
	opMoney
	opCharValue
	opJSONBPathWrite
	opJSONBBuild
	opTimetzComparison
	opBitString
	opNetwork
	opMACAddress
	opJSONBComparison
	opJSONBHash
	opNumericHash
	opJSONBCast
	opJSONBuild
	opJSONConversion
	opTemporalInput
	opTemporalClock
	opTimetz
	opTemporalInRange
	opScalarHash
	opCatalogVector
	opValueHash
	opNestedHash
	opArraySort
	opGeoIO
	opGeoPointBox
	opGeoLinear
	opGeoCircle
	opGeoPathPolygon
	opRangeScalar
	opMultirangeScalar
	opRangeIO
	opRangeHash
	opJSONRecord
	opSettings
	opRegexp
	opFullText
	opAdvisory
	opNativeScalar
	opContextOnly
)

// Only these exact PG18.4 function identities have executable kernels.
func primitiveOperation(id model.FunctionID) primitiveOp {
	switch id {
	case 89, 209, 211, 223, 224, 458, 459, 515, 516, 636, 655, 656,
		657, 658, 659, 676, 710, 716, 717, 745, 746, 766, 768, 769,
		770, 771, 810, 861, 1039, 1063, 1064, 1219, 1236, 1237, 1264, 1305,
		1306, 1307, 1318, 1367, 1372, 1375, 1402, 1403, 1481, 1597, 1598, 1599,
		1619, 1638, 1639, 1713, 1714, 1717, 1738, 1768, 1770, 1772, 1773, 1774,
		1775, 1776, 1777, 1778, 1780, 1810, 1811, 1813, 1845, 1846, 1847, 1965,
		1966, 2003, 2004, 2049, 2071, 2072, 2160, 2161, 2163, 2164, 2166, 2174,
		2175, 2177, 2178, 2180, 2288, 2319, 2338, 2339, 2340, 2341, 2342, 2343,
		2344, 2351, 2352, 2353, 2354, 2355, 2356, 2357, 2364, 2365, 2366, 2367,
		2368, 2369, 2370, 2377, 2378, 2379, 2380, 2381, 2382, 2383, 2401, 2403,
		2417, 2501, 2503, 2520, 2521, 2522, 2523, 2524, 2525, 2526, 2527, 2528,
		2529, 2530, 2531, 2532, 2533, 2546, 2626, 2854, 2855, 2903, 2904, 2905,
		2906, 2907, 2908, 2909, 2910, 2911, 2912, 2913, 2914, 2915, 2916, 2922,
		3078, 3162, 3166, 3181, 3182, 3183, 3184, 3185, 3186, 3187, 3298, 3299,
		3334, 3414, 3432, 3537, 3546, 3696, 3935, 3936, 4126, 4127, 4128, 4129,
		4130, 4131, 4132, 4139, 4140, 4141, 5028, 5091, 6099, 6210, 6211, 6212,
		6215, 6216, 6219, 6220, 6240, 6339, 6340, 6341, 6342, 6364, 6365, 6375,
		6376, 6383, 6384, 6393, 6394, 6412, 6427, 6428, 6429, 6430:
		return opNativeScalar
	case 3, 101, 102, 103, 104, 105, 106, 107, 108, 139, 140, 195,
		196, 197, 198, 208, 222, 267, 268, 276, 323, 326, 327, 330,
		336, 337, 338, 386, 398, 456, 772, 1023, 1024, 1025, 1173, 1250,
		1291, 1300, 1301, 1302, 1303, 1364, 1644, 1645, 1646, 1647, 1648, 1649,
		1650, 1651, 1652, 1653, 1654, 1655, 1814, 1815, 1816, 1817, 1818, 1819,
		1820, 1821, 1822, 1823, 1824, 1825, 1826, 1827, 1828, 1829, 1830, 1831,
		1832, 1833, 1834, 1835, 1836, 1837, 1838, 1839, 1840, 1841, 1842, 1843,
		1844, 1962, 1963, 1964, 2246, 2247, 2248, 2294, 2295, 2296, 2297, 2298,
		2299, 2300, 2301, 2302, 2303, 2304, 2305, 2312, 2313, 2316, 2333, 2334,
		2398, 2399, 2400, 2402, 2404, 2406, 2408, 2410, 2412, 2414, 2416, 2418,
		2420, 2422, 2424, 2426, 2428, 2430, 2432, 2434, 2436, 2456, 2458, 2460,
		2468, 2470, 2472, 2474, 2476, 2478, 2480, 2482, 2484, 2486, 2488, 2490,
		2492, 2494, 2496, 2498, 2500, 2502, 2512, 2513, 2514, 2515, 2516, 2596,
		2598, 2740, 2741, 2746, 2777, 2778, 2785, 2786, 2787, 2804, 2805, 2806,
		2807, 2808, 2809, 2810, 2811, 2812, 2813, 2814, 2815, 2816, 2817, 2858,
		2961, 3097, 3116, 3117, 3120, 3121, 3129, 3130, 3131, 3132, 3133, 3134,
		3135, 3136, 3137, 3157, 3158, 3163, 3169, 3173, 3174, 3178, 3180, 3196,
		3255, 3265, 3266, 3268, 3269, 3283, 3300, 3311, 3312, 3313, 3314, 3324,
		3325, 3328, 3331, 3332, 3333, 3335, 3336, 3337, 3338, 3339, 3340, 3341,
		3342, 3355, 3356, 3357, 3358, 3359, 3387, 3388, 3389, 3390, 3391, 3392,
		3393, 3404, 3405, 3406, 3407, 3434, 3437, 3438, 3446, 3480, 3496, 3497,
		3498, 3499, 3535, 3536, 3543, 3544, 3547, 3548, 3549, 3553, 3554, 3555,
		3557, 3558, 3559, 3560, 3561, 3567, 3568, 3569, 3570, 3571, 3572, 3573,
		3594, 3595, 3646, 3647, 3648, 3649, 3650, 3651, 3652, 3653, 3654, 3686,
		3687, 3688, 3695, 3697, 3698, 3699, 3700, 3701, 3717, 3718, 3719, 3720,
		3721, 3724, 3725, 3726, 3728, 3729, 3731, 3732, 3740, 3741, 3752, 3753,
		3790, 3793, 3795, 3796, 3797, 3798, 3799, 3805, 3816, 3817, 3818, 3832,
		3833, 3836, 3875, 3876, 3879, 3880, 3881, 3916, 3917, 3918, 3944, 3970,
		3971, 3973, 3975, 3977, 3979, 3981, 3983, 3985, 3987, 3989, 3991, 3993,
		3994, 3995, 3996, 4051, 4052, 4226, 4227, 4229, 4230, 4233, 4242, 4243,
		4299, 4300, 4302, 4303, 4304, 4305, 4306, 4307, 4308, 4309, 4310, 4311,
		4312, 4313, 4314, 4315, 4316, 4317, 4318, 4319, 4320, 4321, 4322, 4323,
		4324, 4325, 4326, 4327, 4328, 4329, 4330, 4331, 4332, 4333, 4334, 4335,
		4336, 4337, 4338, 4339, 4340, 4341, 4342, 4343, 4344, 4345, 4346, 4347,
		4348, 4349, 4352, 4353, 4354, 4355, 4356, 4357, 4358, 4359, 4360, 4361,
		4362, 4363, 4364, 4365, 4366, 4367, 4368, 4369, 4370, 4371, 4372, 4373,
		4374, 4375, 4376, 4377, 4378, 4379, 4380, 4381, 4382, 4383, 4384, 4385,
		4386, 4387, 5018, 5019, 5020, 5021, 5033, 5040, 5041, 5050, 5051, 5086,
		5087, 5088, 5089, 5090, 5092, 5093, 5094, 5095, 6098, 6154, 6156, 6179,
		6180, 6225, 6226, 6233, 6234, 6235, 6236, 6242, 6275, 6277, 6278, 6279,
		6283, 6285, 6286, 6287, 6292, 6293, 6294, 6295, 6296, 6297, 6298, 6299,
		6300, 6301, 6306, 6307, 6308, 6324, 6325, 6326, 6345, 6346, 6347, 6354,
		6357, 6362, 6363, 6378, 6379, 6380, 6391, 6397, 6398, 6402, 6403, 6404,
		6405, 6406, 6407, 6408, 6409, 6410:
		return opContextOnly
	case 2880, 2881, 2882, 2883, 2884, 2885, 2886, 2887, 2888, 2889, 2890, 2891, 2892,
		3089, 3090, 3091, 3092, 3093, 3094, 3095, 3096:
		return opAdvisory
	case 3319, 3320, 3321, 3323, 3326, 3327,
		3610, 3611, 3612, 3613, 3616, 3617, 3618, 3619, 3620, 3621, 3622, 3623, 3624, 3625,
		3634, 3635, 3638, 3639, 3640, 3641,
		3662, 3663, 3664, 3665, 3666, 3667, 3668, 3669, 3670, 3671, 3672, 3673, 3684, 3685, 3691, 3692,
		3703, 3704, 3705, 3706, 3707, 3708, 3709, 3710, 3711, 3723,
		3736, 3737, 3738, 3739, 3743, 3744, 3745, 3746, 3747, 3749, 3750, 3751, 3754, 3755, 3756, 3757, 3758, 3759, 3760, 3761, 3768, 3771, 3772, 3773, 3774,
		4201, 4202, 4203, 4204, 4205, 4206, 4207, 4208, 4209, 4210, 4211, 4212, 4213, 4214, 4215, 4216,
		5001, 5003, 5004, 5006, 5007, 5009:
		return opFullText
	case 79, 1238, 1239, 1240, 1241, 1252, 1254, 1256, 1623, 1656, 1657, 1658, 1659, 1986, 1987, 2073, 2074, 2284, 2285, 2767, 2768, 3396, 3397, 6251, 6252, 6253, 6254, 6255, 6256, 6257, 6258, 6259, 6260, 6261, 6262, 6263, 6264, 6265, 6266, 6267, 6268, 6269:
		return opRegexp
	case 2077, 2078, 3294:
		return opSettings
	case 3204, 3209, 3490, 3960, 6338:
		return opJSONRecord
	case 3840, 3841, 3844, 3845, 3848, 3849, 3850, 3851, 3852, 3853, 3854, 3855, 3856, 3857, 3858, 3859, 3860, 3861, 3862, 3863, 3864, 3865, 3866, 3867, 3868, 3869, 3870, 3871, 3872, 3873, 3874, 3914, 3915, 3922, 3923, 3924, 3925, 3928, 3929, 3930, 3933, 3934, 3937, 3938, 3941, 3942, 3945, 3946, 4057, 4401:
		return opRangeScalar
	case 4228, 4235, 4236, 4237, 4238, 4239, 4240, 4241, 4244, 4245, 4246, 4247, 4248, 4249, 4250, 4251, 4252, 4253, 4254, 4255, 4256, 4257, 4258, 4259, 4260, 4261, 4262, 4263, 4264, 4265, 4266, 4267, 4268, 4269, 4270, 4271, 4272, 4273, 4274, 4275, 4276, 4277, 4280, 4281, 4282, 4283, 4284, 4285, 4286, 4287, 4288, 4289, 4290, 4291, 4292, 4293, 4294, 4295, 4296, 4297, 4298, 4388, 4541, 4542:
		return opMultirangeScalar
	case 3834, 3835, 3837, 4231, 4232, 4234:
		return opRangeIO
	case 3417, 3902, 4278, 4279:
		return opRangeHash
	case 117, 118, 119, 120, 121, 122, 123, 124, 347, 348, 1450, 1451, 1490, 1491, 2429, 2481, 2483, 2485, 2487, 2489, 2491:
		return opGeoIO
	case 115, 116, 125, 126, 127, 128, 129, 130, 131, 132, 133, 134, 135, 136, 138, 186, 187, 188, 189, 190, 191, 192, 193, 357, 364, 367, 975, 976, 977, 978, 980, 988, 989, 990, 991, 992, 1406, 1407, 1421, 1422, 1423, 1424, 1425, 1440, 1441, 1442, 1443, 1444, 1534, 1542, 2562, 2563, 2564, 2565, 4067, 4091:
		return opGeoPointBox
	case 225, 239, 277, 278, 361, 362, 363, 365, 366, 368, 369, 372, 373, 380, 381, 702, 704, 725, 727, 959, 960, 961, 981, 993, 994, 995, 996, 997, 998, 999, 1408, 1409, 1410, 1411, 1412, 1413, 1414, 1415, 1482, 1483, 1484, 1485, 1486, 1487, 1488, 1489, 1492, 1493, 1494, 1495, 1496, 1497, 1498, 1499, 1530, 1532, 1541:
		return opGeoLinear
	case 728, 785, 1146, 1147, 1148, 1149, 1416, 1452, 1453, 1454, 1455, 1456, 1457, 1458, 1459, 1460, 1461, 1462, 1463, 1464, 1465, 1466, 1467, 1468, 1469, 1470, 1471, 1472, 1473, 1474, 1475, 1476, 1477, 1478, 1479, 1480, 1543, 1544, 2587, 2588, 3290:
		return opGeoCircle
	case 137, 227, 339, 340, 341, 342, 343, 344, 345, 346, 370, 371, 421, 729, 973, 979, 982, 983, 984, 985, 986, 987, 1426, 1428, 1429, 1430, 1431, 1432, 1433, 1434, 1435, 1436, 1437, 1438, 1439, 1445, 1446, 1447, 1448, 1449, 1531, 1540, 1545, 1556, 2566, 2567, 2568, 2569, 3275, 3292, 5011:
		return opGeoPathPolygon
	case 1688, 1697, 2039, 2963, 3409, 3411, 3412, 3418, 6413, 6414, 6415, 6416, 6417, 6418, 6425, 6426:
		return opValueHash
	case 626, 782, 6192, 6193:
		return opNestedHash
	case 6388, 6389, 6390:
		return opArraySort
	case 40, 41, 54, 55, 404, 457, 619, 677, 678, 679, 680, 681, 776, 2411, 2421:
		return opCatalogVector
	case 400, 425, 441, 442, 443, 444, 445, 447, 448, 449, 450, 451, 452, 453, 455, 949, 972, 1080:
		return opScalarHash
	case 240, 241, 242, 243, 244, 245, 247, 248, 249, 250, 251, 252:
		return opBtreeComparison
	case 1084, 1143, 1150, 1312:
		return opTemporalInput
	case 274, 1299, 2647, 2648, 2649:
		return opTemporalClock
	case 4133, 4134, 4135, 4136, 4137:
		return opTemporalInRange
	case 1271, 1273, 1297, 1298, 1350, 1351, 1359, 1386, 1388, 1696, 1749, 1750, 1969, 2037, 2038, 2046, 2047, 2059, 2547, 3410, 4138, 6201, 6336:
		return opTimetz
	case 4038, 4039, 4040, 4041, 4042, 4043, 4044, 4046, 4050:
		return opJSONBComparison
	case 4045, 3416:
		return opJSONBHash
	case 432, 780:
		return opNumericHash
	case 2580, 3450, 3451, 3452, 3453, 3556:
		return opJSONBCast
	case 3198, 3199, 3200, 3201, 3202, 3203:
		return opJSONBuild
	case 3153, 3154, 3155, 3156, 3176, 3787:
		return opJSONConversion
	case 1564, 1565, 1579, 1580, 1581, 1582, 1592, 1593, 1594, 1595, 1596, 1666, 1667, 1668, 1669, 1670, 1671, 1672, 1673, 1674, 1675, 1676, 1677, 1678, 1679, 1680, 1681, 1682, 1683, 1684, 1685, 1687, 1698, 1699, 1812, 2075, 2076, 2457, 2459, 2902, 2919, 2920, 2921, 3030, 3031, 3032, 3033, 6162:
		return opBitString
	case 422, 598, 599, 605, 635, 683, 696, 697, 698, 699, 711, 730, 779, 910, 911, 920, 921, 922, 923, 924, 925, 926, 927, 928, 929, 930, 1267, 1362, 1427, 1715, 2497, 2499, 2627, 2628, 2629, 2630, 2631, 2632, 2633, 3551, 3562, 3563, 4063, 4071:
		return opNetwork
	case 328, 399, 436, 437, 753, 778, 781, 830, 831, 832, 833, 834, 835, 836, 2495, 3144, 3145, 3146, 3447, 4110, 4111, 4112, 4113, 4114, 4115, 4116, 4117, 4118, 4119, 4120, 4121, 4122, 4123, 4124, 4125:
		return opMACAddress
	case 33, 61, 70, 72, 73, 74, 77, 78, 358, 446, 454, 860, 944, 946, 1245, 1246, 2435:
		return opCharValue
	case 3305, 3579, 5054:
		return opJSONBPathWrite
	case 3263, 3264, 3271, 3272, 3273, 3274:
		return opJSONBBuild
	case 1352, 1353, 1354, 1355, 1356, 1357, 1358, 1379, 1380:
		return opTimetzComparison
	case 1287, 1288, 1400, 1401, 1798, 1799:
		return opOIDName
	case 2405, 2407, 2409, 2413, 2415, 2419, 2423, 2425, 2427, 2431, 2433, 2437, 2473, 6367, 6368, 6369, 6370, 6371, 6372:
		return opBinaryValue
	case 1376, 1732, 1733, 1734, 1735, 1736, 1737, 1739, 1741, 1764, 1766, 1767, 2169, 2461, 2917, 2918, 3281, 3449, 5042, 5043:
		return opNumericExtra
	case 377, 846, 847, 848, 862, 863, 864, 865, 866, 867, 886, 887, 888, 889, 890, 891, 892, 893, 894, 895, 896, 897, 898, 899, 919, 935, 2493, 3344, 3345, 3399, 3811, 3812, 3822, 3823, 3824:
		return opMoney
	case 1151, 1159, 1171, 1174, 1176, 1178, 1189, 1190, 1199, 1217, 1284, 2019, 2027, 2028, 2069, 2549, 3462, 3463, 6203, 6221, 6222, 6223, 6273, 6334, 6335:
		return opTimezone
	case 1304, 1308, 1309, 1310, 1311, 2041, 2042, 2043, 2044:
		return opTemporalOverlaps
	case 6177, 6178:
		return opTimestampBin
	case 2021, 6202:
		return opTimestampFields
	case 2020, 2058:
		return opTimestampCalendar
	case 1385, 6200:
		return opTimeFields
	case 1313:
		return opTimestampOutput
	case 1026, 1152, 1153, 1154, 1155, 1156, 1157, 1158, 1188, 1195, 1196, 1314, 1389, 1967, 2070, 2477:
		return opTimestamp
	case 1272, 1296, 1316, 1961, 2024, 2025, 2029, 2031, 2032, 2033, 2035, 2036, 2045, 2048, 2052, 2053, 2054, 2055, 2056, 2057, 2475, 2548, 3461:
		return opTimestamp
	case 3847, 1102, 1103, 1104, 1105, 1106, 1107, 1144, 1145, 1370, 1377, 1378, 1419, 1690, 1747, 1748, 1848, 1968, 2471:
		return opTime
	case 1384, 6199:
		return opDateFields
	case 1085:
		return opDateOutput
	case 3846, 1086, 1087, 1088, 1089, 1090, 1091, 1092, 1138, 1139, 1140, 1141, 1142, 1373, 2469, 2550:
		return opDate
	case 3261:
		return opJSONStrip
	case 3262:
		return opJSONBStrip
	case 3306:
		return opJSONBPretty
	case 3301:
		return opJSONBConcat
	case 3302, 3303, 3304, 3343:
		return opJSONBDelete
	case 3217, 3940:
		return opJSONBPath
	case 4047, 4048, 4049:
		return opJSONBExists
	case 3478, 3214, 3215, 3216:
		return opJSONBExtract
	case 3951, 3953:
		return opJSONPath
	case 1177, 1180, 2023, 2030, 4001, 4002, 4003, 4004, 4005, 4007, 4008, 4009, 4010, 4011:
		return opJSONPathRuntime
	case 3806, 3804, 3803, 3210, 3207:
		return opJSONB
	case 3947, 3948, 3949, 3950:
		return opJSONTextExtract
	case 321, 322, 324, 3956, 3968:
		return opJSONText
	case 4350, 4351:
		return opUnicodeNormalization
	case 4549, 6105:
		return opUnicodeInfo
	case 6198:
		return opUnicodeString
	case 1268:
		return opParseIdentifier
	case 1172, 6204, 1218:
		return opIntervalField
	case 1197, 1198:
		return opIntervalExtremum
	case 2479:
		return opIntervalSend
	case 1160:
		return opIntervalInput
	case 1161:
		return opIntervalOutput
	case 1162, 1163, 1164, 1165, 1166, 1167, 1168, 1169, 1170, 1175, 1295, 1315, 1326, 1390, 1618, 1624, 2711, 3464:
		return opIntervalArithmetic
	case 1200:
		return opIntervalTypmod
	case 438, 440:
		return opNullCount
	case 109, 110, 2292, 2293:
		return opCString
	case 2290, 2291:
		return opRecordIO
	case 2981, 2982, 2983, 2984, 2985, 2986, 2987:
		return opRecordComparison
	case 3058, 3059, 3539:
		return opVariadicText
	case 3540:
		return opFormatText
	case 1282, 1283, 1289:
		return opTextQuote
	case 1285, 1290:
		return opTypedQuote
	case 34, 35, 266, 406, 407, 408, 409:
		return opNameIO
	case 2952, 2953, 2954, 2955, 2956, 2957, 2958, 2959, 2960, 2962, 6343:
		return opUUID
	case 320, 2170:
		return opWidthBucketRange
	case 3218:
		return opWidthBucketArray
	case 2747, 2748, 2749:
		return opArrayMembership
	case 1193, 1286:
		return opArrayFill
	case 3277, 3278, 3279:
		return opArraySearch
	case 3167, 3168:
		return opArrayReplace
	case 6381:
		return opArrayReverse
	case 378, 379, 383:
		return opArrayConstruction
	case 6172:
		return opArrayTrim
	case 376, 394:
		return opStringToArray
	case 384, 395:
		return opArrayToString
	case 350, 351, 354, 355, 356, 359, 842, 1693, 62, 184, 185,
		2188, 2189, 2190, 2191, 2192, 2193, 2194, 2195, 246, 253:
		return opBtreeComparison
	case 744, 390, 391, 392, 393, 396, 382:
		return opArrayComparison
	case 750, 751:
		return opArrayIO
	case 747, 748, 2091, 2092, 2176, 3179:
		return opArrayMetadata
	case 1404, 1405, 749, 752:
		return opOverlay
	case 721, 722, 723, 724, 6163:
		return opByteAccess
	case 1946, 1947:
		return opBinaryEncoding
	case 2311, 2321, 3419, 3420, 3421, 3422:
		return opDigest
	case 1620, 1621, 2089, 2090, 6330, 6331, 6332, 6333:
		return opTextCode
	case 849, 868, 878, 2014, 2087, 2088:
		return opTextSearch
	case 873, 874, 875, 876, 879, 880, 881, 882, 884, 885, 2015, 6195, 6196:
		return opTextPadding
	case 870, 871, 872, 1622, 3062, 6382:
		return opTextCase
	case 850, 851, 858, 859, 1569, 1570, 1571, 1572, 1631, 1632,
		1633, 1634, 1635, 1636, 1660, 1661, 2005, 2006, 2007, 2008:
		return opLike
	case 1637, 2009:
		return opLikeEscape
	case 5044, 5045, 5048:
		return opGCD
	case 5046, 5047, 5049:
		return opLCM
	case 1892, 1898, 1904:
		return opBitAnd
	case 1893, 1899, 1905:
		return opBitOr
	case 1894, 1900, 1906:
		return opBitXor
	case 1895, 1901, 1907:
		return opBitNot
	case 1896, 1902, 1908:
		return opShiftLeft
	case 1897, 1903, 1909:
		return opShiftRight
	case 1044, 1046:
		return opInput
	case 1045, 1047:
		return opOutput
	case 668, 669:
		return opTypmod
	case 401:
		return opCast
	case 1048:
		return opEq
	case 1049:
		return opLt
	case 1050:
		return opLe
	case 1051:
		return opGt
	case 1052:
		return opGe
	case 1053:
		return opNe
	case 1078:
		return opCmp
	case 1701:
		return opInput
	case 1702:
		return opOutput
	case 1703:
		return opTypmod
	case 1704, 1705:
		return opAbs
	case 1706, 2310:
		return opSign
	case 1707, 1708:
		return opRound
	case 1709, 1710:
		return opTrunc
	case 1711, 2167, 2308, 2320:
		return opCeil
	case 1712, 2309:
		return opFloor
	case 1718:
		return opEq
	case 1719:
		return opNe
	case 1720:
		return opGt
	case 1721:
		return opGe
	case 1722:
		return opLt
	case 1723:
		return opLe
	case 1724:
		return opPl
	case 1725:
		return opMi
	case 1726:
		return opMul
	case 1727:
		return opDiv
	case 1728, 1729:
		return opMod
	case 1730, 1731:
		return opSqrt
	case 1740, 1742, 1743, 1744, 1745, 1746, 1779, 1781, 1782, 1783:
		return opCast
	case 1769:
		return opCmp
	case 1771:
		return opUm
	case 1915:
		return opUp
	case 1973, 1980:
		return opDivTrunc
	case 31, 39, 43, 47, 201, 215, 461, 1243, 2971:
		return opOutput
	case 38, 42, 46, 200, 214, 460, 1242, 1244:
		return opInput
	case 56, 64, 66, 160, 161, 289, 295, 301, 307, 469, 476, 740, 854, 1852, 1858, 1949:
		return opLt
	case 57, 146, 147, 162, 163, 291, 297, 303, 309, 470, 477, 742, 855, 1853, 1859, 1951:
		return opGt
	case 60, 63, 65, 67, 158, 159, 287, 293, 299, 305, 467, 474, 852, 1850, 1856, 1948:
		return opEq
	case 84, 144, 145, 157, 164, 165, 288, 294, 300, 306, 468, 475, 853, 1851, 1857, 1953:
		return opNe
	case 141, 152, 170, 171, 202, 216, 279, 283, 465, 839, 943, 1276, 1280:
		return opMul
	case 148, 149, 166, 167, 290, 296, 302, 308, 471, 478, 741, 856, 1691, 1854, 1860, 1950:
		return opLe
	case 150, 151, 168, 169, 292, 298, 304, 310, 472, 479, 743, 857, 1692, 1855, 1861, 1952:
		return opGe
	case 153, 154, 172, 173, 203, 217, 280, 284, 466, 840, 948, 1277, 1281:
		return opDiv
	case 155, 156, 940, 941, 945, 947:
		return opMod
	case 176, 177, 178, 179, 204, 218, 281, 285, 463, 837, 841, 1274, 1278:
		return opPl
	case 180, 181, 182, 183, 205, 219, 282, 286, 464, 838, 942, 1275, 1279:
		return opMi
	case 206, 212, 213, 220, 462:
		return opUm
	case 207, 221, 1230, 1251, 1253, 1394, 1395, 1396, 1397, 1398:
		return opAbs
	case 228, 1342:
		return opRound
	case 229, 1343:
		return opTrunc
	case 235, 236, 237, 238, 311, 312, 313, 314, 316, 317, 318, 319, 480, 481, 482, 483, 652, 653, 714, 754, 2557, 2558:
		return opCast
	case 720, 1374:
		return opOctets
	case 877, 883, 936, 937, 2012, 2013, 2085, 2086:
		return opSubstr
	case 1257, 1317, 1369, 1381, 2010:
		return opLength
	case 1258, 2011:
		return opCat
	case 1910, 1911, 1912, 1913, 1914:
		return opUp
	case 3060:
		return opLeft
	case 3061:
		return opRight
	case 360, 1954:
		return opCmp
	case 230, 231, 232, 233, 234, 1194, 1339, 1340, 1341, 1344, 1345, 1346, 1347, 1368:
		return opFloatCore
	case 1600, 1601, 1602, 1603, 1604, 1605, 1606, 1607, 1608, 1609, 1610,
		2462, 2463, 2464, 2465, 2466, 2467, 2731, 2732, 2733, 2734, 2735, 2736, 2737, 2738:
		return opFloatTrig
	default:
		return opMissing
	}
}
