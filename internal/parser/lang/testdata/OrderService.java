package com.example.orders;

import java.util.List;
import java.util.Objects;
import static java.util.Objects.requireNonNull;
import java.io.*;

/** Repository contract for order entities. */
public interface OrderRepository<T> extends BaseRepository<T> {
    int DEFAULT_LIMIT = 100;
    T findById(String id);
}

public @interface Audited {
    String value() default "default";
}

public enum OrderStatus {
    PENDING("pending"),
    COMPLETED("completed");

    private final String code;

    OrderStatus(String code) {
        this.code = code;
    }

    public String getCode() {
        return this.code;
    }
}

public record OrderSummary(String id, long totalCents) {
    public boolean isHighValue() {
        return totalCents > 10000;
    }
}

/** Coordinates order lookups, validation, and asynchronous dispatch. */
@Service("orderService")
public class OrderService<T extends BaseEntity> extends AbstractService implements OrderRepository<T>, Serializable {
    private final List<T> entries;
    public static final int MAX_RETRIES = 3;
    private String region, zone;

    /** Constructs an OrderService instance. */
    public OrderService(List<T> entries) {
        super();
        this.entries = requireNonNull(entries);
    }

    public static class Builder {
        public OrderService<BaseEntity> build() {
            return new OrderService<>(List.of());
        }
    }

    @Override
    @Audited("lookup")
    public T findById(String id) {
        Runnable notifier = new Runnable() {
            @Override
            public void run() {
                System.out.println(id);
            }
        };
        entries.stream().map(item -> item.toString()).forEach(System.out::println);
        notifier.run();
        if (id != null && !id.isEmpty() || MAX_RETRIES > 0) {
            try {
                for (T entry : entries) {
                    switch (id) {
                        case "fast":
                            return entry;
                        default:
                            break;
                    }
                }
            } catch (RuntimeException ex) {
                throw ex;
            }
        }
        return null;
    }

    public void tagAll(String... labels) {
        Objects.requireNonNull(labels);
    }
}
