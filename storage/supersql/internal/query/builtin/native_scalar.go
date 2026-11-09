package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

func resolveNativeScalar(signature Signature, options ResolveOptions) (scalarKernel, uint64, error) {
	switch signature.ID {
	case 1770, 1778:
		zone, retained, err := retainTimeZoneContext(options.TimeZone)
		if err != nil {
			return nil, 0, err
		}
		return compileNativeFormat(signature, zone), retained, nil
	case 2338, 2339, 2340, 2341, 2342, 2343, 2344, 2351, 2352, 2353, 2354, 2355, 2356, 2357,
		2364, 2365, 2366, 2367, 2368, 2369, 2370, 2377, 2378, 2379, 2380, 2381, 2382, 2383,
		2520, 2521, 2522, 2523, 2524, 2525, 2526, 2527, 2528, 2529, 2530, 2531, 2532, 2533:
		zone, retained, err := retainTimeZoneContext(options.TimeZone)
		if err != nil {
			return nil, 0, err
		}
		return compileNativeTemporal(signature, zone), retained, nil
	default:
		return compileNativeScalar(signature, options), 0, nil
	}
}

func compileNativeScalar(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 1481, 1738:
		return compileNumericExtra(signature, options)
	case 1305, 1306, 1307:
		return compileTemporalOverlaps(signature)
	case 1768, 1772, 1773, 1774, 1775, 1776, 1777, 1780, 2049:
		return compileNativeFormat(signature, namedTimeZone{})
	case 2071, 2072, 2338, 2339, 2340, 2341, 2342, 2343, 2344, 2351, 2352, 2353, 2354, 2355, 2356, 2357,
		2364, 2365, 2366, 2367, 2368, 2369, 2370, 2377, 2378, 2379, 2380, 2381, 2382, 2383,
		2520, 2521, 2522, 2523, 2524, 2525, 2526, 2527, 2528, 2529, 2530, 2531, 2532, 2533, 2546:
		return compileNativeTemporal(signature, namedTimeZone{})
	case 4126, 4127, 4128, 4129, 4130, 4131, 4132, 4139, 4140, 4141:
		return compileNativeInRange(signature)
	default:
		return compileNativeBasic(signature)
	}
}

func nativeScalarTyped(id model.FunctionID) bool {
	switch id {
	case 515, 516, 1619, 2003, 2004, 2333, 2334, 2401, 2403, 2503, 2804, 3162, 3173, 3180, 3181, 3182, 3183, 3184, 3185, 3186, 3187, 3265, 3268, 3414, 3547, 4051, 4052, 5028, 5091, 6215, 6216, 6275, 6277, 6278, 6279, 6283, 6285, 6286, 6287, 6292, 6375, 6376:
		return true
	default:
		return false
	}
}

