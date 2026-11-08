cmake_minimum_required(VERSION 3.21)
project(chrome_net_server LANGUAGES C CXX)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

# Locate HQQ root
get_filename_component(CHROME_NET_SERVER_DIR "${CMAKE_CURRENT_SOURCE_DIR}" ABSOLUTE)
get_filename_component(HQQ_C_DIR "${CHROME_NET_SERVER_DIR}/../../../../ipc/hqq/HQQ/c" ABSOLUTE)

set(HQQ_BUILD_TESTS OFF CACHE BOOL "" FORCE)
set(HQQ_BUILD_CXX_CHECK OFF CACHE BOOL "" FORCE)
set(HQQ_BUILD_STRESS OFF CACHE BOOL "" FORCE)

if(EXISTS "${HQQ_C_DIR}/CMakeLists.txt")
  add_subdirectory("${HQQ_C_DIR}" hqq_subbuild EXCLUDE_FROM_ALL)
else()
  message(FATAL_ERROR "HQQ C directory not found at ${HQQ_C_DIR}")
endif()

# Core static library
add_library(chrome_engine_core STATIC
  signatures.cc
  engine_core.cc
  webtransport_adapter.cc
  websocket_adapter.cc
)

target_include_directories(chrome_engine_core PUBLIC
  "${CMAKE_CURRENT_SOURCE_DIR}"
  "${HQQ_C_DIR}/include"
)

# Native server executable
add_executable(chrome_net_server
  main.c
  chrome_net_server.c
)

target_include_directories(chrome_net_server PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}"
  "${HQQ_C_DIR}/include"
)

target_link_libraries(chrome_net_server PRIVATE
  chrome_engine_core
  hqq::hqq
)

# Test executable
enable_testing()
add_executable(signatures_test
  signatures_test.cc
)

target_include_directories(signatures_test PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}"
  "${HQQ_C_DIR}/include"
)

target_link_libraries(signatures_test PRIVATE
  chrome_engine_core
  hqq::hqq
)

add_test(NAME signatures_test COMMAND signatures_test)
