---
author: Lee Yunjin (@GoSuda Rivulet)
---

# Flink Stream Operator Basics: Focus on Stream Processing

# What is the main use of Flink?

**Flink is a purpose-built distributed computing framework designed to handle heavy workloads.**

Flink README briefly introduces itself: "Apache Flink is an open source stream processing framework with powerful stream- and batch-processing capabilities."
However, this introduction may be abstract to understand basic philosophy of the project.

So, briefly: 
- Flink excels at handling real-time data processing pipelines.
- It distributes computation across cluster nodes for high throughput and low latency.
- It relies on small, logically simple operators which are composed into larger workflows.

Basically, Flink needs processes consisting of small operators.
Each operator is logically simple.
For example, a user wants to filter logs that have ID 100.
The user has to generate two streams for alertmanager, mailman while operating Union between two.

```java

import org.apache.flink.api.common.functions.FilterFunction;
import org.apache.flink.streaming.api.datastream.DataStream;
import org.apache.flink.streaming.api.environment.StreamExecutionEnvironment;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;


public class JsonFilterUnionExample {

    public static void main(String[] args) throws Exception {
        StreamExecutionEnvironment env = StreamExecutionEnvironment.getExecutionEnvironment();

        // Source stream: for example, read JSON strings from socket
        DataStream<String> sourceStream = env.socketTextStream("localhost", 9999);

        ObjectMapper mapper = new ObjectMapper();

        // Filter records where ID equals 100
        DataStream<String> id100Stream = sourceStream.filter(new FilterFunction<String>() {
            @Override
            public boolean filter(String value) throws Exception {
                JsonNode node = mapper.readTree(value);
                return node.has("ID") && node.get("ID").asInt() == 100;
            }
        });

        // Filter records where type is "alertmanager"
        // This datastream is derived from id100Stream.
        DataStream<String> alertManagerStream = id100Stream.filter(new FilterFunction<String>() {
            @Override
            public boolean filter(String value) throws Exception {
                JsonNode node = mapper.readTree(value);
                return node.has("type") && node.get("type").asText().equals("alertmanager");
            }
        }); //Stream saves each log into nodes. node.has("type") filters proper nodes.

        // Filter records where type is "mailman".
        DataStream<String> mailmanStream = id100Stream.filter(new FilterFunction<String>() {
            @Override
            public boolean filter(String value) throws Exception {
                JsonNode node = mapper.readTree(value);
                return node.has("type") && node.get("type").asText().equals("mailman");
            }
        }); 

        // Union the two filtered streams
        DataStream<String> unionStream = alertManagerStream.union(mailmanStream);

        // Print the results
        unionStream.print();

        env.execute("JSON Filter and Union Example");
    }
}

```

The client app is written by dividing the required algorithm into given units of operators.
Then Flink distributes each operator into cluster nodes by generating DAG (Directed Acyclic Graph).

This enhances performance as the workloads are well distributed (DAG is one of the most efficient ways to generate distribution graphs; many distributed networks are proof of this).

So, before we understand the key points of distribution, we should examine the smallest operators.

### The function we use is NOT the core implementation.

```java
    @SafeVarargs
    public final DataStream<T> union(DataStream<T>... streams) {
        List<Transformation<T>> unionedTransforms = new ArrayList<>();
        unionedTransforms.add(this.transformation);

        for (DataStream<T> newStream : streams) {
            if (!getType().equals(newStream.getType())) {
                throw new IllegalArgumentException(
                        "Cannot union streams of different types: "
                                + getType()
                                + " and "
                                + newStream.getType());
            }

            unionedTransforms.add(newStream.getTransformation());
        }
        return new DataStream<>(this.environment, new UnionTransformation<>(unionedTransforms));
    }

```

This function simply calls another operation.

The same structure repeats for each operation.

### How does UnionTransformation class work

Unfortunately, this class also calls lots of external functions.
At least, we can guess that Union operator may simply collect two different streams, and stream it to new single stream. Like tiny streams are making rivers, core logic of union is **a flow**. Also, union joins two data streams with the same types, without complex implementation; so we should check for DAG edge to see a clearer implementation.

```java

 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package org.apache.flink.streaming.api.transformations;

import org.apache.flink.annotation.Internal;
import org.apache.flink.api.dag.Transformation;

import org.apache.flink.shaded.guava33.com.google.common.collect.Lists;

import java.util.ArrayList;
import java.util.List;
import java.util.stream.Collectors;

/**
 * This transformation represents a union of several input {@link Transformation Transformations}.
 *
 * <p>This does not create a physical operation, it only affects how upstream operations are
 * connected to downstream operations.
 *
 * @param <T> The type of the elements that result from this {@code UnionTransformation}
 */
@Internal
public class UnionTransformation<T> extends Transformation<T> {
    private final List<Transformation<T>> inputs;

    /**
     * Creates a new {@code UnionTransformation} from the given input {@code Transformations}.
     *
     * <p>The input {@code Transformations} must all have the same type.
     *
     * @param inputs The list of input {@code Transformations}
     */
    public UnionTransformation(List<Transformation<T>> inputs) {
        super("Union", inputs.get(0).getOutputType(), inputs.get(0).getParallelism());

        for (Transformation<T> input : inputs) {
            if (!input.getOutputType().equals(getOutputType())) {
                throw new UnsupportedOperationException("Type mismatch in input " + input);
            }
        }

        this.inputs = Lists.newArrayList(inputs);
    }

    @Override
    public List<Transformation<?>> getInputs() {
        return new ArrayList<>(inputs);
    }

    @Override
    protected List<Transformation<?>> getTransitivePredecessorsInternal() {
        List<Transformation<?>> predecessors =
                inputs.stream()
                        .flatMap(input -> input.getTransitivePredecessors().stream())
                        .distinct()
                        .collect(Collectors.toList());
        predecessors.add(this);
        return predecessors;
    }
}
```


### Next Steps

Union is a simple example of Flink's distribution strategy. This simply joins many streams into a single, big stream. 
However, most of the operations are performed on different types. We should know how different streams are unified in Flink.

To develop a similar framework in Go language, coding stream types and operator units will be the first step.
Also, Go sets itself apart from traditional OOP. 
Project structure should be modified while keeping the key points.
### Recommended Structure
- Define a stream type interface with dummy operator chaining
- Implement operator units while splitting shared properties into separate units, as Go does not support traditional inheritance.
- Build a graph manager to construct DAGs from instructions
- Write distribution schedulers while considering system resources
